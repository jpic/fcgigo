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
	"log"
)

// fcgiRequest holds the state for an in-progress request,
// and provides functions for converting back and forth between fcgiRequest and http.Request
type fcgiRequest struct {
	reqId     uint16
	params    map[string]string
	stdin     *bytes.Buffer
	data      *bytes.Buffer
	close     bool
	startTime int64
	conn      io.ReadWriteCloser
}

func newFcgiRequest(reqid uint16, conn io.ReadWriteCloser, flags uint8) *fcgiRequest {
	return &fcgiRequest{
		reqId: reqid,
		params: map[string]string{},
		stdin: new(bytes.Buffer),
		data: new(bytes.Buffer),
		close: ((flags & FCGI_KEEP_CONN) != FCGI_KEEP_CONN),
		startTime: time.Nanoseconds(),
		conn: conn,
	}
}
func getFcgiRequest(reqid uint16, conn *http.Conn, req *http.Request) *fcgiRequest {
	self := &fcgiRequest{
		reqId: reqid,
		// close: req.Close, // this is not right because whether the ws<->fcgi connection stays up is separate from whether the browser<->ws connection goes down
		close: false, // by default we want to keep the connection open
		params: map[string]string{
			"SERVER_SOFTWARE": "fcgigo-server",
			"HTTP_HOST": req.Host,
			"SERVER_NAME": req.Host,
			"REQUEST_URI": req.RawURL,
			"REQUEST_METHOD": req.Method,
			"GATEWAY_INTERFACE": "FCGI/1.0",
			"SERVER_PORT": "0", //TODO
			"SERVER_ADDR": "127.0.0.1",
			"SERVER_PROTOCOL": req.Proto,
			"REMOTE_PORT": "0",
			"REMOTE_ADDR": conn.RemoteAddr,
			"SCRIPT_FILENAME": "", // TODO: this wouldnt be defined for a remote responder, but would if it were spawned. deferred until we support dynamic spawning again (ie have unit tests).
			"SCRIPT_NAME": req.URL.Path, // TODO: this should be the path portion of the url matched by the ServeMux pattern
			"PATH_INFO": "", // TODO: this should be the remainder of the path AFTER the ServeMux pattern is stripped from the front
			"DOCUMENT_ROOT": "",
			"PATH_TRANSLATED": "", // depends on PATH_INFO and DOCUMENT_ROOT
			"QUERY_STRING": req.URL.RawQuery,
		},
		stdin: new(bytes.Buffer),
		data: new(bytes.Buffer),
	}
	// set a default DOCUMENT_ROOT
	if dir, err := os.Getwd(); err != nil {
		self.params["DOCUMENT_ROOT"] = dir
	}
	// patch the ?query_string to include the #fragment
	if len(req.URL.Fragment) > 0 {
		Log("FcgiConn: adding fragment to query string")
		self.params["QUERY_STRING"] = self.params["QUERY_STRING"] + "#" + req.URL.Fragment
	}
	// carry over the content-length
	if c, ok := req.Header["Content-Length"]; ok {
		Log("FcgiConn: Setting content-length")
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
		Log("Saving Param:", k, v)
		self.params[k] = v
	}
	return self
}
func (self *fcgiRequest) getHttpRequest() (h *http.Request) {
	h = &http.Request{
		Method: self.params["REQUEST_METHOD"],
		RawURL: self.params["REQUEST_URI"],
		Proto: self.params["SERVER_PROTOCOL"],
		Close: self.close,
		Body: nopCloser{Reader: self.stdin},
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
			h.Header[standardCase(strings.Bytes(key)[5:])] = val
		}
	}
	return h
}
func (self *fcgiRequest) parseFcgiParams(text []byte) {
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
		Log("Param:", key, val)
	}
}

// fcgiListener is a net.Listener that you can pass to http.Serve(),
// http.Serve() will then, in effect, be running a FastCGI Responder
type fcgiListener struct {
	net.Listener
	c   chan *rsConn
	err chan os.Error
}

// Listen() creates a new fcgiListener of the specified net type.
// Known values for net are: "tcp", "unix", and "exec".
// For tcp, laddr is like "127.0.0.1:1234".
// For unix, laddr is the absolute path to a socket.
// For exec, laddr is ignored (input is read from stdin).
func Listen(net string, laddr string) (net.Listener, os.Error) {
	switch net {
	case "tcp", "tcp4", "tcp6":
		return ListenTCP(laddr)
	case "unix":
		return ListenUnix(laddr)
	case "exec":
		return ListenFD(FCGI_LISTENSOCK_FILENO)
	}
	return nil, os.NewError("Invalid network type.")
}

// ListenTCP() creates a new fcgiListener on a tcp socket.
// listenAddress can be any resolvable local interface and port.
func ListenTCP(listenAddress string) (net.Listener, os.Error) {
	var err os.Error
	if l, err := net.Listen("tcp", listenAddress); err == nil {
		return listen(l)
	}
	return nil, err
}

// ListenUnix creates a new fcgiListener on a unix socket.
// socketPath should be the absolute path to the socket file.
func ListenUnix(socketPath string) (net.Listener, os.Error) {
	if l, err := net.Listen("unix", socketPath); err == nil {
		if l, err = listen(l); err == nil {
			log.Stderr("ListenUnix returning", l, nil)
			return l, nil
		} else {
			return nil, err
		}
		return l, err
	} else {
		return nil, err
	}
	panic()
}

// ListenFD creates a new fcgiListener on an already open socket.
// fd is the file descriptor of the open socket.
func ListenFD(fd int) (net.Listener, os.Error) {
	return listen(NewFDListener(fd))
}

// listen() is the private listener factory behind the different net types
func listen(listener net.Listener) (net.Listener, os.Error) {
	self := &fcgiListener{
		Listener: listener,
		c: make(chan *rsConn),
		err: make(chan os.Error, 1),
	}
	// start a goroutine that calls Accept on the real listener
	// then starts reading packets from it until it's complete enough to Accept
	go func() {
		for {
			if self == nil || self.Listener == nil {
				break
			}
			if c, err := self.Listener.Accept(); err == nil {
				go self.readAllPackets(c) // once enough packets have been read, fcgiListener.Accept() will yield a connection
			} else {
				self.err <- err
				break
			}
		}
	}()
	return self, nil
}

// readAllPackets is the goroutine that will read FCGI records off the real socket
// and dispatch the rsConns to Accept() when they are ready
func (self *fcgiListener) readAllPackets(conn io.ReadWriteCloser) {
	requests := map[uint16]*fcgiRequest{}
	h := &fcgiHeader{}
	for {
		h.Version = 0 // mark the packet as invalid
		err := readStruct(conn, h)
		switch {
		case err == os.EOF:
			Log("Listener: EOF")
			goto close
		case err != nil:
			Log("Listener: error reading fcgiHeader", err)
			goto close
		case h.Version != 1:
			Log("Listener: got an invalid fcgiHeader", h)
			goto close
		}
		req, _ := requests[h.ReqId]
		switch h.Kind {
		case FCGI_BEGIN_REQUEST:
			b := new(fcgiBeginRequest)
			readStruct(conn, b)
			Log("Listener: got FCGI_BEGIN_REQUEST", b)
			req = newFcgiRequest(h.ReqId, conn, b.Flags)
			requests[h.ReqId] = req
		case FCGI_PARAMS:
			Log("Listener: got FCGI_PARAMS")
			if content, err := h.readContent(conn); err == nil {
				req.parseFcgiParams(content)
			} else {
				Log("Error reading content:", err)
			}
		case FCGI_STDIN:
			Log("Listener: got FCGI_STDIN", h.ContentLength, "bytes")
			if h.ContentLength == uint16(0) {
				// now the request has enough data to build our fake http.Conn
				self.c <- newRsConn(conn, req)
				// this will cause Accept() to trigger and release an rsConn for the server to use
			} else {
				if content, err := h.readContent(conn); err == nil {
					req.stdin.Write(content)
				} else {
					Log("Error reading content:", err)
				}
			}
		case FCGI_GET_VALUES:
			Log("Listener: FCGI_GET_VALUES")
			// TODO: respond with GET_VALUES_RESULT
		case FCGI_DATA:
			Log("Listener: got FCGI_DATA", h.ContentLength, "bytes")
			if h.ContentLength > uint16(0) {
				if content, err := h.readContent(conn); err == nil {
					req.data.Write(content)
				} else {
					Log("Error reading content:", err)
				}
			}
		case FCGI_ABORT_REQUEST:
			Log("Listener: ABORT_REQUEST")
			// can we pre-empt the worker go-routine? punt for now.
			// spec says we should answer by ending the output streams
			// req.fcgiWrite(FCGI_STDOUT, "")
			// req.fcgiWrite(FCGI_STDERR, "")
			// but really, since the goroutine is still running,
			// and will still send its own close messages
			// pick your poison: either send too many closes,
			// or send them late (with spurious data in between)
			// punting again.
		default:
			Log("Listener: Unknown packet header type: %d in %s", h.Kind, h)
			fcgiWrite(conn, FCGI_UNKNOWN_TYPE, h.ReqId, []byte{h.Kind, 0, 0, 0, 0, 0, 0, 0})
		}
	}
close:
	Log("Listener: calling conn.Close()")
	conn.Close()
}

// Accept() returns a rsConn as a net.Conn interface.
// Will only return rsConns that are ready to Read a complete http.Request from.
func (self *fcgiListener) Accept() (net.Conn, os.Error) {
	select {
	case c := <-self.c:
		Log("Listener: Accept() releasing connection", c)
		return net.Conn(c), nil
	case err := <-self.err:
		return nil, err
	}
	return nil, nil
}

// rsConn is the responder-side of a connection to the webserver, it looks like a net.Conn
// It is created automatically by readAllPackets() and returned by fcgiListener.Accept() from inside http.Serve().
// It won't be created until a complete request has been buffered off a real socket.
// Read() here will read from that request buffer only, never a real socket.
//  - Its possible that in the future calls to Read() would block waiting for chunks of FCGI_STDIN,etc to arrive
// Write() will send FCGI_STDOUT records back to the web server.
type rsConn struct {
	reqId      uint16             // the request id to put in the headers of the output packets
	conn       io.ReadWriteCloser // the ReadWriteCloser to do real I/O on
	buf        *bytes.Buffer      // the buffer that .Read() will read from
	remoteAddr net.Addr           // this should be the address of the far remote, the user's IP at their browser
	localAddr  net.Addr           // this is undefined, is it the webserver or the responder? but its required
	close      bool               // whether, when closed, to also close conn
	closedOut  bool               // did the EOF STDOUT messages get sent already?
	closedErr  bool               // did the EOF STDERR messages get sent already?
}

func newRsConn(conn io.ReadWriteCloser, req *fcgiRequest) *rsConn {
	f := &rsConn{
		reqId: req.reqId,
		conn: conn,
		buf: new(bytes.Buffer),
		remoteAddr: &net.TCPAddr{net.IPv4(127, 0, 0, 1), 0}, // TODO
		localAddr: &net.TCPAddr{net.IPv4(127, 0, 0, 1), 0}, // TODO
		close: req.close,
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
	return fcgiWrite(self.conn, FCGI_STDOUT, self.reqId, p)
}

func (self *rsConn) fcgiWrite(kind uint8, str string) (n int, err os.Error) {
	if kind == FCGI_STDOUT && self.closedOut {
		return 0, nil
	} else if kind == FCGI_STDERR && self.closedErr {
		return 0, nil
	}
	if len(str) == 0 {
		switch kind {
		case FCGI_STDOUT:
			self.closedOut = true
		case FCGI_STDERR:
			self.closedErr = true
		}
	}
	return fcgiWrite(self.conn, kind, self.reqId, strings.Bytes(str))
}

// ASSUMPTION:
// The http server always closes the connection right now.
// We use this to end the fcgi request (and close the socket if .close is set).
// But, once the http server supports keep-alive connections,
// this assumption will no longer hold, and http.Conn will need to expose its endRequest or something
func (self *rsConn) Close() os.Error {
	Log("rsConn: Closed. Sending END messages.")
	// send the done messages
	if !self.closedOut {
		self.fcgiWrite(FCGI_STDOUT, "")
	}
	if !self.closedErr {
		self.fcgiWrite(FCGI_STDERR, "")
	}
	// write the final packet
	writeStruct(self.conn, newFcgiHeader(FCGI_END_REQUEST, self.reqId, 8))
	writeStruct(self.conn, (&fcgiEndRequest{
		AppStatus: 200,
		ProtocolStatus: FCGI_REQUEST_COMPLETE,
	}))
	// did the webserver request that we close this connection
	if self.close {
		self.conn.Close()
	}
	Log("rsConn: Done closing.")
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

// FDListener is a net.Listener that uses syscall.Accept(fd)
// to accept new connections directly on an already open fd.
// This is needed by FCGI because if we are dynamically spawned,
// the webserver will open the socket for us.
type FDListener struct {
	fd int
}

// ListenFD creates an FDListener, which listens on the given fd.
// fd must refer to an already open socket
func NewFDListener(fd int) *FDListener { return &FDListener{fd: fd} }

// Accept() blocks until a new connection is available on our fd
// returns a FileConn as a net.Conn for Listener interface
func (self *FDListener) Accept() (c net.Conn, err os.Error) {
	if nfd, _, e := syscall.Accept(self.fd); e == 0 {
		c = FileConn{os.NewFile(nfd, "<fd:"+strconv.Itoa(self.fd)+">")}
	} else {
		err = os.NewError("Syscall error:" + strconv.Itoa(e))
	}
	return c, err
}

// Close() closes the fd
// in the context of fcgi, a webserver's response to this is undefined, it may terminate our process
// but in any case, this process will no longer accept new connections from the webserver
func (self *FDListener) Close() os.Error {
	if err := syscall.Close(self.fd); err == 0 {
		return os.NewError("Syscall.Close error:" + strconv.Itoa(err))
	}
	return nil
}
func (self *FDListener) Addr() net.Addr { return &FDAddr{Fd: self.fd} }

// In order to Accept() new connections from an fd, we need some wrappers:

// FileConn takes an os.File and provides a net.Conn interface
type FileConn struct {
	*os.File
}

func (f FileConn) LocalAddr() net.Addr  { return FDAddr{Fd: f.Fd()} }
func (f FileConn) RemoteAddr() net.Addr { return FileAddr{Path: f.Name()} }
func (f FileConn) SetTimeout(ns int64) os.Error {
	return nil
}
func (f FileConn) SetReadTimeout(ns int64) os.Error {
	return nil
}
func (f FileConn) SetWriteTimeout(ns int64) os.Error {
	return nil
}

// FileAddr is the "address" when we are connected to a FileConn,
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
