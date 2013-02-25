package daemon

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrStopped is returned when Accept is called on a listener
// which has been stopped.
var ErrStopped = errors.New("daemon: listener stopped")

// ErrTimeout is returned when Restart times out.
var ErrTimeout = errors.New("daemon: timeout")

type waitConn struct {
	*sync.WaitGroup
	net.Conn
}

func (c *waitConn) Close() error {
	defer c.Done()
	Verbose.Printf("Closed connection: (local) %s <- %s (remote)",
		c.LocalAddr(), c.RemoteAddr())
	return c.Conn.Close()
}

// A WaitListener is a listener which accepts connections like a normal
// Listener, but counts them and can Wait for all of them to close.
type WaitListener struct {
	wg sync.WaitGroup
	net.Listener
	stop chan bool
}

// Accept is a wrapper around the underlying Listener's accept
// to facilitate tracking connections.
func (w *WaitListener) Accept() (conn net.Conn, err error) {
	// To prevent race conditions, always assume we're going
	// to accept a connection.
	w.wg.Add(1)
	defer func() {
		// If we didn't accept, decrement the count ourselves
		if conn == nil {
			w.wg.Done()
		}
	}()

	select {
	case <-w.stop:
		return nil, ErrStopped
	default:
	}

	conn, err = w.Listener.Accept()
	if err != nil {
		if strings.Contains(err.Error(), "closed network connection") {
			return nil, ErrStopped
		}
		return nil, err
	}

	Verbose.Printf("Accepted connection: (local) %s <- %s (remote)",
		conn.LocalAddr(), conn.RemoteAddr())

	return &waitConn{
		WaitGroup: &w.wg,
		Conn:      conn,
	}, nil
}

// Close stops and closes the listener; it is an error to close more than once.
func (w *WaitListener) Close() error {
	close(w.stop)

	Verbose.Printf("Closing listener: %s", w.Addr())
	return w.Listener.Close()
}

// Stop stops the listener so that it can be used in another process.  After
// Stop, it may be necessary to create a dummy connection to this Listener to
// fall out of an existing Accept.  It is an error to call Stop more than once.
func (w *WaitListener) Stop() {
	close(w.stop)

	Verbose.Printf("Stopping listener: %s", w.Addr())
}

// Dup copies the listener's underlying file descriptor.  This is intended to
// be used to pass the file descriptor on to a restarted version of this
// process.
func (w *WaitListener) Dup() int {
	tcp, ok := w.Listener.(*net.TCPListener)
	if !ok {
		Fatal.Printf("unknown listener type: %T", w.Listener)
	}

	lf, err := tcp.File()
	if err != nil {
		Fatal.Printf("failed to get fd: %s", err)
	}
	fd := lf.Fd()

	newFD, err := dup(int(fd))
	if err != nil {
		Fatal.Printf("failed to dup(%d): %s", fd, err)
	}
	return newFD

}

// Wait waits for all associated connections to close.
func (w *WaitListener) Wait() {
	w.wg.Wait()
}

// noop makes a dummy connection to the listener
func (w *WaitListener) noop() {
	addr := w.Addr().(*net.TCPAddr)
	for _, ip := range []net.IP{
		net.IPv4(127, 0, 0, 1),
		net.IPv6loopback,
		addr.IP,
	} {
		addr.IP = ip
		conn, err := net.DialTCP("tcp", nil, addr)
		if err != nil {
			Verbose.Printf("noop(%q): %s", addr, err)
			continue
		}
		defer conn.Close()
		Verbose.Printf("noop(%q): Success", addr)
		return
	}
	Verbose.Printf("noop(%q): failed to ping", addr)
}

// A Listenable is something which can listen.  It can either
// be backed by a file descriptor of an existing listener,
// or if none is available, a new listener.  String returns
// the intended address for the listening socket as a string.
type Listenable interface {
	Listen() (net.Listener, error)
	String() string
}

type listenFlag struct {
	flag, proto string
	mode        string // "fd", "tcp"

	// mode == "fd"
	fd       int
	listener *WaitListener

	// mode == "tcp"
	net   string
	laddr *net.TCPAddr
}

func (l *listenFlag) Listen() (net.Listener, error) {
	var under net.Listener
	var err error
	switch l.mode {
	case "fd":
		f := os.NewFile(uintptr(l.fd), fmt.Sprintf("&%d", l.fd))
		under, err = net.FileListener(f)
	case "tcp":
		under, err = net.ListenTCP(l.net, l.laddr)
	default:
		return nil, fmt.Errorf("unknown mode %q", l.mode)
	}
	if err != nil {
		return nil, err
	}
	Verbose.Printf("Listening for %s on: %s (from %s)", l.proto, under.Addr(), l.mode)
	listener := &WaitListener{
		Listener: under,
		stop:     make(chan bool),
	}
	l.listener = listener
	return listener, nil
}

func (l *listenFlag) String() string {
	return l.laddr.String()
}

func (l *listenFlag) Set(s string) error {
	if len(s) == 0 {
		return fmt.Errorf("--%s requires an argument", l.flag)
	}

	// Check for passed file descriptor
	if s[0] == '&' {
		fd, err := strconv.Atoi(s[1:])
		if err != nil {
			return fmt.Errorf("failed to parse &fd: %s", err)
		}
		l.mode, l.fd = "fd", fd
		return nil
	}

	laddr, err := net.ResolveTCPAddr(l.net, s)
	if err != nil {
		return fmt.Errorf("failed to resolve %q: %s", s, err)
	}
	l.mode, l.laddr = "tcp", laddr
	return nil
}

// ListenFlag registers a flag, which, when set, causes the returned
// Listenable to listen on the provided address.  If the flag is not
// provided, the default addr will be used.  The given proto is used
// to create the help text.
func ListenFlag(name, netw, addr, proto string) Listenable {
	laddr, err := net.ResolveTCPAddr(netw, addr)
	if err != nil {
		Fatal.Printf("failed to resolve default %q: %s", addr, err)
	}

	f := &listenFlag{
		flag:  name,
		proto: proto,
		mode:  "tcp",
		net:   netw,
		laddr: laddr,
	}
	flag.Var(f, name, fmt.Sprintf("Address on which to listen for %s", proto))
	return f
}

func copyFlags() (arg0 string, flags []string, ports []*WaitListener) {
	arg0 = os.Args[0]
	flag.VisitAll(func(f *flag.Flag) {
		if lf, ok := f.Value.(*listenFlag); ok && lf.listener != nil {
			fd := lf.listener.Dup()
			ports = append(ports, lf.listener)
			flags = append(flags, fmt.Sprintf("--%s=&%d", f.Name, fd))
			return
		}
		flags = append(flags, fmt.Sprintf("--%s=%s", f.Name, f.Value))
	})
	return
}

func spawn(arg0 string, flags []string) {
	Verbose.Printf("Spawning process: %q %q", arg0, flags)
	cmd := exec.Command(arg0, flags...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		Fatal.Printf("Exec failed: %s", err)
	}
}

// Restart re-execs the current process, passing all of the same flags,
// except that ListenFlags will be replaced with "&fd" to copy the file
// descriptor from this process.  Restart does not return.
func Restart(timeout time.Duration) {
	arg0, flags, ports := copyFlags()
	for _, w := range ports {
		w.Stop()
		// Send noop connections to free up the accept loops
		w.noop()
	}

	spawn(arg0, flags)

	// Wait for all connections to close out
	done := make(chan bool)
	go func() {
		defer close(done)
		for _, w := range ports {
			w.Wait()
		}
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		Fatal.Printf("Restart timed out after %s", timeout)
	}
	Verbose.Printf("Restart complete")
	os.Exit(0)
}

// Shutdown closes all ListenFlags and waits for their connections to
// finish.  Shutdown does not return.
func Shutdown(timeout time.Duration) {
	_, _, ports := copyFlags()
	for _, w := range ports {
		w.Close()
	}

	// Wait for all connections to close out
	done := make(chan bool)
	go func() {
		defer close(done)
		for _, w := range ports {
			w.Wait()
		}
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		Fatal.Printf("Shutdown timed out after %s", timeout)
	}
	Info.Printf("Shutdown complete")
	os.Exit(0)
}

// LameDuck specifies the duration of the lame duck mode after the
// listener is closed before the binary exits.
var LameDuck = 15 * time.Second

// Run is the last thing to call from main.  It does not return.
//
// Run handles the following signals:
//   SIGINT    - Calls Shutdown
//   SIGTERM   - Calls Shutdown
//   SIGHUP    - Calls Restart
//   SIGUSR1   - Dumps a stack trace to the logs
//
// If another signal is received during Shutdown or Restart, the process
// will terminate immediately.
func Run() {
	incoming := make(chan os.Signal, 10)
	signal.Notify(incoming, signals...)
	for sig := range incoming {
		switch sigAction(sig) {
		case sigShutdown:
			go Shutdown(LameDuck)
			<-incoming
			Fatal.Printf("Shutdown aborted")
		case sigRestart:
			go Restart(LameDuck)
			<-incoming
			Fatal.Printf("Restart aborted")
		case sigStackDump:
			V(-5).Printf("Stack dump:\n" + stack())
		default:
			Warning.Printf("Unknown signal: %s", sig)
		}
	}
}

// Return values for platform-specific sigAction
const (
	sigUnknown = iota
	sigShutdown
	sigRestart
	sigStackDump
)