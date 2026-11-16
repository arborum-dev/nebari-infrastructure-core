package rke2

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// notExistExit is the exit status ReadFile's script uses for a missing file.
const notExistExit = 66

// dialTimeout bounds the TCP connect and SSH handshake; tests shorten it.
var dialTimeout = 30 * time.Second

// SSHOptions configures an SSH connection to the target host.
type SSHOptions struct {
	Host           string
	Port           int
	User           string
	PrivateKeyPath string
	KnownHostsPath string
	HostKeyAlias   string // when set, looked up in known_hosts instead of host:port
}

type sshRunner struct {
	client *ssh.Client
	sudo   bool
}

// Dial connects to the host with public-key authentication. The host key must
// already be present in KnownHostsPath; it is never added automatically.
func Dial(ctx context.Context, opts SSHOptions) (Runner, error) {
	keyPEM, err := os.ReadFile(opts.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading PrivateKeyPath %q: %w", opts.PrivateKeyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing PrivateKeyPath %q: %w", opts.PrivateKeyPath, err)
	}
	check, err := knownhosts.New(opts.KnownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("reading known_hosts %q: %w", opts.KnownHostsPath, err)
	}

	port := opts.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(opts.Host, strconv.Itoa(port))

	display, lookup := addr, addr
	if opts.HostKeyAlias != "" {
		display = opts.HostKeyAlias
		lookup = net.JoinHostPort(opts.HostKeyAlias, "22") // known_hosts stores a default-port entry as the bare name
	}
	callback := func(_ string, remote net.Addr, key ssh.PublicKey) error {
		err := check(lookup, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) {
			if len(keyErr.Want) == 0 {
				return fmt.Errorf("host %s is not in known_hosts file %s; add its key (for example with ssh-keyscan) after verifying it: %w",
					display, opts.KnownHostsPath, err)
			}
			return fmt.Errorf("host key for %s does not match known_hosts file %s; refusing to connect: %w",
				display, opts.KnownHostsPath, err)
		}
		return fmt.Errorf("verifying host key for %s against %s: %w", display, opts.KnownHostsPath, err)
	}

	cfg := &ssh.ClientConfig{
		User:            opts.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: callback,
	}
	// Offer only key types known_hosts holds for this host. The callback
	// above still verifies the key the server presents.
	if algs, err := knownAlgorithms(check, lookup); err == nil {
		cfg.HostKeyAlgorithms = algs
	}

	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", addr, err)
	}

	// NewClientConn ignores ClientConfig.Timeout, so bound the handshake here
	// and abort it if ctx is cancelled.
	if err := conn.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("setting handshake deadline for %s: %w", addr, err)
	}
	handshakeDone := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-handshakeDone:
		}
	}()
	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	close(handshakeDone)
	<-watcherDone
	if err != nil {
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("ssh handshake with %s: %w", addr, ctxErr)
		}
		var neg *ssh.AlgorithmNegotiationError
		if len(cfg.HostKeyAlgorithms) > 0 && errors.As(err, &neg) && neg.What == "host key" {
			return nil, fmt.Errorf("host %s offers no host key of a type known_hosts file %s lists for it (%s); "+
				"its host key may have changed, so refusing to connect: %w",
				display, opts.KnownHostsPath, strings.Join(cfg.HostKeyAlgorithms, ", "), err)
		}
		return nil, fmt.Errorf("ssh handshake with %s: %w", addr, err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = cc.Close()
		return nil, fmt.Errorf("clearing handshake deadline for %s: %w", addr, err)
	}
	return &sshRunner{client: ssh.NewClient(cc, chans, reqs), sudo: opts.User != "root"}, nil
}

// knownAlgorithms returns the host key algorithms known_hosts lists for
// lookup, de-duplicated and in file order, or nil when the host is unlisted.
// It probes check with a throwaway key, which can never match: knownhosts
// reports the keys it holds for the host in the resulting KeyError.
func knownAlgorithms(check ssh.HostKeyCallback, lookup string) ([]string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	probe, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	err = check(lookup, &net.TCPAddr{IP: net.IPv4zero}, probe.PublicKey())
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) {
		return nil, nil
	}
	var algs []string
	seen := map[string]bool{}
	for _, k := range keyErr.Want {
		names := []string{k.Key.Type()}
		if k.Key.Type() == ssh.KeyAlgoRSA {
			names = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
		}
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				algs = append(algs, n)
			}
		}
	}
	return algs, nil
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (r *sshRunner) command(script string) string {
	cmd := "sh -c " + shellQuote(script)
	if r.sudo {
		return "sudo -n " + cmd
	}
	return cmd
}

func (r *sshRunner) exec(ctx context.Context, script string, stdin []byte) (string, error) {
	sess, err := r.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("opening ssh session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Close()
		case <-done:
		}
	}()

	err = sess.Run(r.command(script))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout.String(), ctxErr
	}
	if err != nil {
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			return stdout.String(), &ExitError{Code: exit.ExitStatus(), Stderr: stderr.String()}
		}
		return stdout.String(), fmt.Errorf("running command: %w", err)
	}
	return stdout.String(), nil
}

func (r *sshRunner) Run(ctx context.Context, script string) (string, error) {
	return r.exec(ctx, script, nil)
}

func (r *sshRunner) ReadFile(ctx context.Context, p string) ([]byte, error) {
	q := shellQuote(p)
	out, err := r.exec(ctx, fmt.Sprintf("[ -e %s ] || exit %d; cat %s", q, notExistExit, q), nil)
	var ee *ExitError
	if errors.As(err, &ee) && ee.Code == notExistExit {
		return nil, &fs.PathError{Op: "read", Path: p, Err: fs.ErrNotExist}
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", p, err)
	}
	return []byte(out), nil
}

func (r *sshRunner) WriteFile(ctx context.Context, p string, data []byte, mode fs.FileMode) error {
	q := shellQuote(p)
	script := fmt.Sprintf(
		`mkdir -p %s && tmp=$(mktemp %s) && { cat >"$tmp" && chmod %o "$tmp" && mv -f "$tmp" %s; } || { rc=$?; rm -f "$tmp"; exit $rc; }`,
		shellQuote(path.Dir(p)), shellQuote(p+".XXXXXX"), mode.Perm(), q)
	if data == nil {
		data = []byte{}
	}
	if _, err := r.exec(ctx, script, data); err != nil {
		return fmt.Errorf("writing %s: %w", p, err)
	}
	return nil
}

func (r *sshRunner) Close() error { return r.client.Close() }
