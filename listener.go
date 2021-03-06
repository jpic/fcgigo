// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The FastCGI Listener and Conn types that get used by the http server.

package fcgi

import (
	"os"
	"io"
	"net"
	"bytes"
	"http"
	"time"
	"strings"
	"syscall"
	"strconv"
	"fmt"
	"encoding/binary"
)

// request holds the state for an in-progress request,
// and provides functions for converting back and forth between request and http.Request
type request struct {
	reqId     uint16
	params    map[string]string
	stdin     *bytes.Buffer
	data      *bytes.Buffer
	close     bool
	startTime int64
	conn      io.ReadWriteCloser
}

func newRequest(reqid uint16, conn io.ReadWriteCloser, flags uint8) *request {
	return &request{
		reqId:     reqid,
		params:    map[string]string{},
		stdin:     new(bytes.Buffer),
		data:      new(bytes.Buffer),
		close:     ((flags & flagKeepConn) != flagKeepConn),
		startTime: time.Nanoseconds(),
		conn:      conn,
	}
}
func getRequest(reqid uint16, conn *http.Conn, req *http.Request) *request {
	self := &request{
		reqId: reqid,
		// close: req.Close, // this is not right because whether the ws<->fcgi connection stays up is separate from whether the browser<->ws connection goes down
		close: false, // by default we want to keep the connection open
		params: map[string]string{
			"SERVER_SOFTWARE":   "fcgigo-server",
			"HTTP_HOST":         req.Host,
			"SERVER_NAME":       req.Host,
			"REQUEST_URI":       req.RawURL,
			"REQUEST_METHOD":    req.Method,
			"GATEWAY_INTERFACE": "FCGI/1.0",
			"SERVER_PORT":       "0", //TODO
			"SERVER_ADDR":       "127.0.0.1",
			"SERVER_PROTOCOL":   req.Proto,
			"REMOTE_PORT":       "0",
			"REMOTE_ADDR":       conn.RemoteAddr,
			"SCRIPT_FILENAME":   "",           // TODO: this wouldnt be defined for a remote responder, but would if it were spawned. deferred until we support dynamic spawning again (ie have unit tests).
			"SCRIPT_NAME":       req.URL.Path, // TODO: this should be the path portion of the url matched by the ServeMux pattern
			"PATH_INFO":         "",           // TODO: this should be the remainder of the path AFTER the ServeMux pattern is stripped from the front
			"DOCUMENT_ROOT":     "",
			"PATH_TRANSLATED":   "", // depends on PATH_INFO and DOCUMENT_ROOT
			"QUERY_STRING":      req.URL.RawQuery,
		},
		stdin: new(bytes.Buffer),
		data:  new(bytes.Buffer),
	}
	// set a default DOCUMENT_ROOT
	if dir, err := os.Getwd(); err != nil {
		self.params["DOCUMENT_ROOT"] = dir
	}
	// patch the ?query_string to include the #fragment
	if len(req.URL.Fragment) > 0 {
		self.params["QUERY_STRING"] = self.params["QUERY_STRING"] + "#" + req.URL.Fragment
	}
	// carry over the content-length
	if c, ok := req.Header["Content-Length"]; ok {
		self.params["CONTENT_LENGTH"] = c
	}
	// store the HTTP_HEADER_NAME version of each header
	for k, v := range req.Header {
		k = strings.Map(func(c int) int {
			if c == '-' {
				return '_'
			}
			return c
		},
			"HTTP_"+strings.ToUpper(k))
		self.params[k] = v
	}
	return self
}
func (self *request) getHttpRequest() (h *http.Request) {
	h = &http.Request{
		Method: self.params["REQUEST_METHOD"],
		RawURL: self.params["REQUEST_URI"],
		Proto:  self.params["SERVER_PROTOCOL"],
		Close:  self.close,
		Body:   nopCloser{Reader: self.stdin},
		Header: map[string]string{},
	}
	if h.Proto[0:4] != "HTTP" {
		return nil
	}
	i := 5
	h.ProtoMajor, i, _ = atoi(h.Proto, i)
	h.ProtoMinor, _, _ = atoi(h.Proto, i+1)
	if url, err := http.ParseURLReference("http://" + self.params["HTTP_HOST"] + self.params["REQUEST_URI"] + "?" + self.params["QUERY_STRING"]); err == nil {
		h.URL = url
	}
	if host, ok := self.params["HTTP_HOST"]; ok {
		h.Host = host
	}
	if ref, ok := self.params["HTTP_REFERER"]; ok {
		h.Referer = ref
	}
	if agent, ok := self.params["HTTP_USER_AGENT"]; ok {
		h.UserAgent = agent
	}
	for key, val := range self.params {
		if strings.HasPrefix(key, "HTTP_") {
			h.Header[standardCase([]byte(key)[5:])] = val
		}
	}
	return h
}
func (self *request) parseFcgiParams(text []byte) {
	// parseFcgiParams reads an encoded []byte into Params
	slice := text[0:len(text)]
	for len(slice) > 0 {
		var (
			key_len uint32 = 0
			val_len uint32 = 0
			key     string = ""
			val     string = ""
		)
		key_len, slice = getOneSize(slice)
		val_len, slice = getOneSize(slice)
		key, slice = getOneValue(slice, key_len)
		val, slice = getOneValue(slice, val_len)
		self.params[key] = val
	}
}

// listener is a net.Listener that you can pass to http.Serve(),
// http.Serve() will then, in effect, be running a FastCGI Responder
type listener struct {
	net.Listener
	net string // tcp, unix, or exec
	c   chan *rsConn
	err chan os.Error
}

// Listen() creates a new listener of the specified net type.
// Known values for net are: "tcp", "unix", and "exec".
// For tcp, laddr is like "127.0.0.1:1234".
// For unix, laddr is the absolute path to a socket.
// For exec, laddr is ignored (input is read from stdin).
func Listen(net string, laddr string) (net.Listener, os.Error) {
	switch net {
	case "tcp", "tcp4", "tcp6":
		return listenTCP(laddr)
	case "unix":
		return listenUnix(laddr)
	case "exec":
		return listenFD(listenSockFileNo)
	}
	return nil, os.NewError(fmt.Sprint("Invalid network type.", net))
}

// listenTCP() creates a new listener on a tcp socket.
// listenAddress can be any resolvable local interface and port.
func listenTCP(listenAddress string) (net.Listener, os.Error) {
	var err os.Error
	if l, err := net.Listen("tcp", listenAddress); err == nil {
		ret, err := listen("tcp", l)
		return ret, err
	}
	return nil, err
}

// listenUnix creates a new listener on a unix socket.
// socketPath should be the absolute path to the socket file.
func listenUnix(socketPath string) (net.Listener, os.Error) {
	if err := os.Remove(socketPath); err != nil {
		// there has to be a better way...
		switch err.String() {
		case "remove " + socketPath + ": no such file or directory":
		default:
			return nil, err
		}
	}
	if l, err := net.Listen("unix", socketPath); err == nil {
		if ll, err := listen("unix", l); err == nil {
			return ll, nil
		} else {
			return nil, err
		}
	} else {
		return nil, err
	}
	panic("listenUnix should not fall-through")
}

// listenFD creates a new listener on an already open socket.
// fd is the file descriptor of the open socket.
func listenFD(fd int) (net.Listener, os.Error) {
	ret, err := listen("exec", newFDListener(fd))
	return ret, err
}

// listen() is the private listener factory behind the different net types
func listen(net string, l net.Listener) (*listener, os.Error) {
	if l == nil {
		return nil, os.NewError("listener cannot be nil")
	}
	self := &listener{
		Listener: l,
		net:      net,
		c:        make(chan *rsConn, 1),
		err:      make(chan os.Error, 1),
	}
	// start a goroutine that calls Accept on the real listener
	// then starts reading packets from it until it's complete enough to Accept
	go func() {
		for {
			if c, err := self.Listener.Accept(); err == nil {
				lc := new(lockReadWriteCloser)
				lc.ReadWriteCloser = c
				go self.readAllPackets(lc)
			} else {
				self.err <- err
				break
			}
		}
	}()
	if net == "exec" {
		// there is no finalizer that is guaranteed to run before the parent process exits
		// so when the webserver dies, we cant send kill signals to the spawned processes
		// so here, in the exec'd child, we start a slow-poll to check if our parent has died
		go func() {
			for {
				time.Sleep(7e9)
				if os.Getppid() == 1 {
					os.Exit(1)
				}
			}
		}()
	}
	return self, nil
}

// readAllPackets is the goroutine that will read FCGI records off the conn
// and dispatch the rsConns to Accept() when they are ready
func (self *listener) readAllPackets(conn *lockReadWriteCloser) {
	requests := map[uint16]*request{}
	h := new(header)
	locked := false
	for {

		// lock the connection until we read the header AND content
		// we have to do it in 2 reads, because we dont know how much to read
		// until after we inspect the header, and no one else can read in-between
		conn.r.Lock()
		locked = true // for cleaning up the lock later,
		// cant simply defer conn.r.Unlock(), because it has to toggle per-loop

		// read the header
		err := binary.Read(conn, binary.BigEndian, h)

		// check for errors
		switch {
		case err == os.EOF:
			goto disconnected
		case err != nil:
			self.Log("error reading header", err)
			goto disconnected
		case h.Version != 1:
			self.Log("got an invalid header", h)
			goto disconnected
		}

		// get the request, if any
		req, _ := requests[h.ReqId]
		// we dont check if req exists because this might be the beginRequest for it

		// check the kind of packet
		switch h.Kind {
		case typeBeginRequest:
			b := new(beginRequest)
			binary.Read(conn, binary.BigEndian, b)
			// self.Log("got beginRequest", h.ReqId)
			req = newRequest(h.ReqId, conn, b.Flags)
			requests[h.ReqId] = req
		case typeParams:
			if content, err := h.readContent(conn); err == nil {
				req.parseFcgiParams(content)
			} else {
				self.Log("Error reading typeParams content:", err)
			}
		case typeStdin:
			if h.ContentLength == uint16(0) {
				// now the request has enough data to build our fake http.Conn
				self.c <- newRsConn(conn, req)
				// this will cause Accept() to trigger and release an rsConn for the server to use
				// if no other goroutine has called Accept(), then this
			} else {
				if content, err := h.readContent(conn); err == nil {
					req.stdin.Write(content)
				} else {
					self.Log("Error reading typeStdincontent:", err)
				}
			}
		case typeGetValues:
			self.Log("TODO: typeGetValues")
			// TODO: respond with GET_VALUES_RESULT
		case typeData:
			if h.ContentLength > uint16(0) {
				if content, err := h.readContent(conn); err == nil {
					req.data.Write(content)
				} else {
					self.Log("Error reading content:", err)
				}
			}
		case typeAbortRequest:
			self.Log("ABORT_REQUEST")
			// can we pre-empt the worker go-routine? punt for now.
			// spec says we should answer by ending the output streams
			// req.endStream(typeStdout)
			// req.endStream(typeStderr)
			// but really, since the goroutine is still running,
			// and will still send its own close messages
			// pick your poison: either send too many closes,
			// or send them late (with spurious data in between)
			// punting again.
		default:
			self.Log("Unknown packet header type: %d in %s", h.Kind, h)
			writeRecord(conn, typeUnknownType, h.ReqId, []byte{h.Kind, 0, 0, 0, 0, 0, 0, 0})
		}

		// unlock the connection
		conn.r.Unlock()
		locked = false
	}
disconnected:
	if locked {
		conn.r.Unlock()
	}
	conn.Close()
}

// Accept() returns a rsConn as a net.Conn interface.
// Will only return rsConns that are ready to Read a complete http.Request from.
func (self *listener) Accept() (net.Conn, os.Error) {
	select {
	case c := <-self.c:
		if c == nil {
			return nil, os.NewError("Listener: Can't accept a nil connection.")
		}
		return net.Conn(c), nil
	case err := <-self.err:
		if err == nil {
			return nil, os.NewError("Unknown error in Accept(), a nil error was sent on the error channel.")
		}
		return nil, err
	}
	panic("Accept should never fall through")
}

func (self *listener) Close() os.Error {
	self.Log("Close()")
	return self.Listener.Close()
}

func (self *listener) Log(msg string, v ...) {
	msg = fmt.Sprintf("Listener(%s): %s", self.net, msg)
	Log(msg, v)
}


// rsConn is the responder-side of a connection to the webserver, it looks like a net.Conn.
// It is created automatically by readAllPackets() and returned by listener.Accept() from inside http.Serve().
// It won't be created until a complete request has been buffered off a real socket.
// Read() here will read from that request buffer only, never a real socket.
//  - Its possible that in the future calls to Read() would be unbuffered and block waiting for chunks of typeStdin,etc to arrive.
// Write() will send typeStdout records back to the web server.
type rsConn struct {
	reqId      uint16               // the request id to put in the headers of the output packets
	conn       *lockReadWriteCloser // the ReadWriteCloser to do real I/O on
	buf        *bytes.Buffer        // the buffer that .Read() will read from
	remoteAddr net.Addr             // this should be the address of the far remote, the user's IP at their browser
	localAddr  net.Addr             // this is undefined, is it the webserver or the responder? but its required
	close      bool                 // whether, when closed, to also close conn
	closedOut  bool                 // did the EOF STDOUT messages get sent already?
	closedErr  bool                 // did the EOF STDERR messages get sent already?
}

func newRsConn(conn *lockReadWriteCloser, req *request) *rsConn {
	f := &rsConn{
		reqId:      req.reqId,
		conn:       conn,
		buf:        new(bytes.Buffer),
		remoteAddr: &net.TCPAddr{net.IPv4(127, 0, 0, 1), 0}, // TODO
		localAddr:  &net.TCPAddr{net.IPv4(127, 0, 0, 1), 0}, // TODO
		close:      req.close,
	}
	// fill our buffer with the output of the given http.Request
	req.getHttpRequest().Write(f.buf)
	return f
}

func (self *rsConn) Read(p []byte) (n int, err os.Error) {
	return self.buf.Read(p)
}

func (self *rsConn) Write(p []byte) (n int, err os.Error) {
	if self.closedOut {
		return 0, os.EOF
	}
	if len(p) == 0 {
		self.closedOut = true
	}
	return len(p), writeRecord(self.conn, typeStdout, self.reqId, p)
}

func (self *rsConn) endStream(kind uint8) (err os.Error) {
	if kind == typeStdout && self.closedOut {
		return nil
	} else if kind == typeStderr && self.closedErr {
		return nil
	}
	switch kind {
	case typeStdout:
		self.closedOut = true
	case typeStderr:
		self.closedErr = true
	}
	return writeRecord(self.conn, kind, self.reqId, []byte{})
}

// ASSUMPTION:
// The http server always closes the connection right now.
// We use this to end the fcgi request (and close the socket if .close is set).
// But, once the http server supports keep-alive connections,
// this assumption will no longer hold, and http.Conn will need to expose its endRequest or something
func (self *rsConn) Close() os.Error {
	// Log("rsConn: Close() -> sending CLOSE and END messages for this request.")
	// send the done messages
	if !self.closedOut {
		self.endStream(typeStdout)
	}
	if !self.closedErr {
		self.endStream(typeStderr)
	}
	// write the final packet
	if _, err := writeEndRequest(self.conn, self.reqId, 200, statusRequestComplete); err != nil {
		return err
	}
	// did the webserver request that we close this connection
	if self.close {
		Log("rsConn: Close()ing real connection to web-server.")
		self.conn.Close()
	}
	return nil
}

func (self *rsConn) LocalAddr() net.Addr  { return self.localAddr }
func (self *rsConn) RemoteAddr() net.Addr { return self.remoteAddr }
func (self *rsConn) SetTimeout(nsec int64) os.Error {
	return nil
}
func (self *rsConn) SetReadTimeout(nsec int64) os.Error {
	return nil
}
func (self *rsConn) SetWriteTimeout(nsec int64) os.Error {
	return nil
}
func (self *rsConn) String() string {
	return fmt.Sprint("{rsConn@", self.localAddr.String(), " reqId:", self.reqId, " read buffer:", self.buf.Len(), "}")
}

// fdListener is a net.Listener that uses syscall.Accept(fd)
// to accept new connections directly on an already open fd.
// This is needed by FCGI because if we are dynamically spawned,
// the webserver will open the socket for us.
type fdListener struct {
	fd int
}

// listenFD creates an fdListener, which listens on the given fd.
// fd must refer to an already open socket
func newFDListener(fd int) *fdListener { return &fdListener{fd: fd} }

// Accept() blocks until a new connection is available on our fd
// returns a fileConn as a net.Conn for Listener interface
func (self *fdListener) Accept() (c net.Conn, err os.Error) {
	if nfd, _, e := syscall.Accept(self.fd); e == 0 {
		c = fileConn{os.NewFile(nfd, "<fd:"+strconv.Itoa(self.fd)+">")}
	} else {
		err = os.NewError("Syscall error:" + strconv.Itoa(e))
	}
	return c, err
}

// Close() closes the fd
// in the context of fcgi, a webserver's response to this is undefined, it may terminate our process
// but in any case, this process will no longer accept new connections from the webserver
func (self *fdListener) Close() os.Error {
	if err := syscall.Close(self.fd); err == 0 {
		return os.NewError("Syscall.Close error:" + strconv.Itoa(err))
	}
	return nil
}
func (self *fdListener) Addr() net.Addr { return &FDAddr{Fd: self.fd} }

// In order to Accept() new connections from an fd, we need some wrappers:

// fileConn takes an os.File and provides a net.Conn interface
type fileConn struct {
	*os.File
}

func (f fileConn) LocalAddr() net.Addr  { return FDAddr{Fd: f.Fd()} }
func (f fileConn) RemoteAddr() net.Addr { return FileAddr{Path: f.Name()} }
func (f fileConn) SetTimeout(ns int64) os.Error {
	return nil
}
func (f fileConn) SetReadTimeout(ns int64) os.Error {
	return nil
}
func (f fileConn) SetWriteTimeout(ns int64) os.Error {
	return nil
}
func (f fileConn) String() string {
	return fmt.Sprint("{fileConn@ fd:", f.Fd(), " name:", f.Name(), "}")
}

// FileAddr is the "address" when we are connected to a fileConn,
// the path to the file (or the name passed to Open() if an fd file)
type FileAddr struct {
	Path string
}

func (f FileAddr) Network() string { return "file" }
func (f FileAddr) String() string  { return "file://" + f.Path }

// FDAddr is the address when we connect directly to an already open fd
type FDAddr struct {
	Fd int
}

func (f FDAddr) Network() string { return "exec" }
func (f FDAddr) String() string  { return "exec://" + strconv.Itoa(f.Fd) }
