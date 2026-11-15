package rke2

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type execRecord struct {
	cmd   string
	stdin []byte
}

// reply is what the test server answers to an exec request.
type reply struct {
	stdout string
	stderr string
	code   int
}

type testServer struct {
	t       *testing.T
	addr    string
	port    int
	hostKey ssh.PublicKey
	keyPath string // client private key
	handler func(cmd string) reply

	mu    sync.Mutex
	execs []execRecord
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func writeClientKey(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return path, sp
}

func startServer(t *testing.T, handler func(cmd string) reply) *testServer {
	t.Helper()
	keyPath, clientPub := writeClientKey(t)
	hostSigner := newSigner(t)

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(clientPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unauthorized key")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{
		t:       t,
		addr:    ln.Addr().String(),
		port:    ln.Addr().(*net.TCPAddr).Port,
		hostKey: hostSigner.PublicKey(),
		keyPath: keyPath,
		handler: handler,
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, cfg)
		}
	}()
	return s
}

func (s *testServer) serve(c net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		_ = c.Close()
		return
	}
	defer func() { _ = conn.Close() }()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go s.session(ch, creqs)
	}
}

func (s *testServer) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			return
		}
		_ = req.Reply(true, nil)
		stdin, _ := io.ReadAll(ch)
		s.mu.Lock()
		s.execs = append(s.execs, execRecord{cmd: payload.Command, stdin: stdin})
		s.mu.Unlock()

		r := s.handler(payload.Command)
		_, _ = io.WriteString(ch, r.stdout)
		_, _ = io.WriteString(ch.Stderr(), r.stderr)
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(r.code)})) //nolint:gosec // test codes are small
		return
	}
}

func (s *testServer) lastExec(t *testing.T) execRecord {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.execs) == 0 {
		t.Fatal("server received no exec request")
	}
	return s.execs[len(s.execs)-1]
}

// knownHostsFile writes a known_hosts file holding key under each host entry.
func knownHostsFile(t *testing.T, key ssh.PublicKey, hosts ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	var sb strings.Builder
	for _, h := range hosts {
		sb.WriteString(knownhosts.Line([]string{h}, key) + "\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (s *testServer) opts(user, knownHosts string) SSHOptions {
	return SSHOptions{
		Host:           "127.0.0.1",
		Port:           s.port,
		User:           user,
		PrivateKeyPath: s.keyPath,
		KnownHostsPath: knownHosts,
	}
}

func dialOK(t *testing.T, s *testServer, user string) Runner {
	t.Helper()
	kh := knownHostsFile(t, s.hostKey, s.addr)
	r, err := Dial(context.Background(), s.opts(user, kh))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func okHandler(string) reply { return reply{stdout: "ok"} }

func TestDial_UnknownHostKeyRefused(t *testing.T) {
	s := startServer(t, okHandler)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Dial(context.Background(), s.opts("root", kh))
	if err == nil {
		t.Fatal("Dial succeeded with unknown host key")
	}
	for _, want := range []string{"127.0.0.1", kh} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	after, rerr := os.ReadFile(kh) //nolint:gosec // path from t.TempDir()
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(after) != 0 {
		t.Errorf("known_hosts was modified: %q", after)
	}
}

func TestDial_ChangedHostKeyRefused(t *testing.T) {
	s := startServer(t, okHandler)
	kh := knownHostsFile(t, newSigner(t).PublicKey(), s.addr)

	_, err := Dial(context.Background(), s.opts("root", kh))
	if err == nil {
		t.Fatal("Dial succeeded with changed host key")
	}
	if !strings.Contains(err.Error(), kh) {
		t.Errorf("error %q does not name %s", err, kh)
	}
}

func TestDial_HostKeyAlias(t *testing.T) {
	s := startServer(t, okHandler)
	kh := knownHostsFile(t, s.hostKey, "lima-colima-rke2")
	opts := s.opts("root", kh)
	opts.HostKeyAlias = "lima-colima-rke2"

	r, err := Dial(context.Background(), opts)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = r.Close()
}

func TestRun_RootRunsDirectly(t *testing.T) {
	s := startServer(t, okHandler)
	r := dialOK(t, s, "root")

	out, err := r.Run(context.Background(), "echo hi")
	if err != nil || out != "ok" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	cmd := s.lastExec(t).cmd
	if strings.Contains(cmd, "sudo") || !strings.Contains(cmd, "echo hi") {
		t.Errorf("command = %q", cmd)
	}
}

func TestRun_NonRootUsesNonInteractiveSudo(t *testing.T) {
	s := startServer(t, okHandler)
	r := dialOK(t, s, "lima")

	if _, err := r.Run(context.Background(), "echo hi"); err != nil {
		t.Fatal(err)
	}
	cmd := s.lastExec(t).cmd
	if !strings.HasPrefix(cmd, "sudo -n ") || !strings.Contains(cmd, "echo hi") {
		t.Errorf("command = %q", cmd)
	}
}

func TestRun_ExitError(t *testing.T) {
	s := startServer(t, func(string) reply { return reply{stderr: "boom", code: 3} })
	r := dialOK(t, s, "root")

	_, err := r.Run(context.Background(), "false")
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if ee.Code != 3 || !strings.Contains(ee.Stderr, "boom") {
		t.Errorf("ExitError = %+v", ee)
	}
	if !strings.Contains(ee.Error(), "3") || !strings.Contains(ee.Error(), "boom") {
		t.Errorf("Error() = %q", ee.Error())
	}
}

func TestReadFile_NotExist(t *testing.T) {
	s := startServer(t, func(string) reply { return reply{code: notExistExit} })
	r := dialOK(t, s, "root")

	_, err := r.ReadFile(context.Background(), "/etc/rancher/rke2/config.yaml")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestReadFile_ReturnsContent(t *testing.T) {
	s := startServer(t, func(string) reply { return reply{stdout: "data"} })
	r := dialOK(t, s, "root")

	got, err := r.ReadFile(context.Background(), "/x")
	if err != nil || string(got) != "data" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
}

func TestWriteFile_SendsContentAndMode(t *testing.T) {
	s := startServer(t, okHandler)
	r := dialOK(t, s, "root")

	path := "/etc/rancher/rke2/config.yaml"
	data := []byte("token: abc\n")
	if err := r.WriteFile(context.Background(), path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := s.lastExec(t)
	if string(rec.stdin) != string(data) {
		t.Errorf("stdin = %q, want %q", rec.stdin, data)
	}
	if !strings.Contains(rec.cmd, path) || !strings.Contains(rec.cmd, "600") {
		t.Errorf("command = %q", rec.cmd)
	}
}

func TestDial_MissingPrivateKey(t *testing.T) {
	s := startServer(t, okHandler)
	kh := knownHostsFile(t, s.hostKey, s.addr)
	opts := s.opts("root", kh)
	opts.PrivateKeyPath = filepath.Join(t.TempDir(), "absent")

	_, err := Dial(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "PrivateKeyPath") {
		t.Fatalf("err = %v, want mention of PrivateKeyPath", err)
	}
}

func TestRun_ContextCancelled(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	s := startServer(t, func(string) reply { <-block; return reply{} })
	r := dialOK(t, s, "root")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := r.Run(ctx, "sleep 100"); done <- err }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
