/*
	fcgi.go is a FastCGI implementation in pure Go.

	It is meant to integrate directly with the http module.

	It provides two main pieces: fcgi.Handler and fcgi.Listener

	fcgi.Handler() returns a http.Handler that will do round-robin dispatch to remote FastCGI Responders to handle each request.
	fcgi.Listener() is a net.Listener that you can pass to http.Serve to produce a FastCGI Responder.

	Responder Example:
		// register your http handlers exactly like normal
		http.Handle("/hello", http.HandlerFunc(func(con *http.Conn, req *http.Request) {
			io.WriteString(con, "hello, world!\n")
		}))

		// then start the responder server
		if listen, err := fcgi.Listen("0.0.0.0:7134"); err == nil {
			http.Serve(listen, nil)
			// now if you configured lighttpd to connect to ( "host" => "127.0.0.1", "port" => 7134 ),
			// it will serve your hello, world application
		}

	Or, you could stick with all Go, and use fcgi.Handler from another host/process.

	Handler Example:
		// register a http.Handler that will dispatch to our responders
		http.Handle("/hello", fcgi.Handler([]string{
			"127.0.0.1:7134",
			// ... repeat ...
		}))

		http.ListenAndServe(":80", nil)

		// in theory, this should work with any FCGI Responder (not just ones created by fcgi.Listener).
		// need tests to prove this

	See fcgi_test.go for examples of how to use both the Handler and Listener in the same process.
	(hint: they cannot share the same default muxer).

	Future Work:

	There should be a variant of fcgi.Handler that can use ForkAndExec to dynamically spawn another process,
	and connect to it over an FD.  fcgi.ListenFD is in place to provide support for accepting connections this way.

*/
package fcgi

import (
	"os"
	"io"
	"io/ioutil"
	"net"
	"encoding/binary"
	"bytes"
	"strings"
	"strconv"
	"http"
	"time"
	"syscall"
	// "log"
	"fmt"
	"bufio"
)

// Socket is a handy short-hand for ReadWriteCloser.
// Because FastCGI has to work over a variety of connection types (tcp, unix, and stdin), we need to keep our I/O fairly abstract.
type Socket io.ReadWriteCloser

var Log = dontLog             // or = log.Stderr to see the logging output
func dontLog(k string, v ...) {}

// The FastCGI protocol definitions:

// Packet Types (fcgiHeader.Kind)
const (
	FCGI_BEGIN_REQUEST = iota + 1
	FCGI_ABORT_REQUEST
	FCGI_END_REQUEST
	FCGI_PARAMS
	FCGI_STDIN
	FCGI_STDOUT
	FCGI_STDERR
	FCGI_DATA
	FCGI_GET_VALUES
	FCGI_GET_VALUES_RESULT
	FCGI_UNKNOWN_TYPE
	FCGI_MAXTYPE = FCGI_UNKNOWN_TYPE
)
// keep the connection between web-server and responder open after request
const FCGI_KEEP_CONN = 1
// max amount of data in a FastCGI record body
const FCGI_MAX_WRITE = 65534
// Roles (fcgiBeginRequest.Roles)
const (
	FCGI_RESPONDER = iota + 1 // only Responders are implemented.
	FCGI_AUTHORIZER
	FCGI_FILTER
)
// ProtocolStatus (in fcgiEndRequest)
const (
	FCGI_REQUEST_COMPLETE = iota
	FCGI_CANT_MPX_CONN
	FCGI_OVERLOADED
	FCGI_UNKNOWN_ROLE
)

type fcgiHeader struct {
	Version       uint8
	Kind          uint8
	ReqId         uint16
	ContentLength uint16
	PaddingLength uint8
	Reserved      uint8
}
type fcgiEndRequest struct {
	AppStatus      uint32
	ProtocolStatus uint8
	Reserved       [3]uint8
}
type fcgiBeginRequest struct {
	Role     uint16
	Flags    uint8
	Reserved [5]uint8
}

func newFcgiHeader(kind uint8, id uint16, content_length int) *fcgiHeader {
	return &fcgiHeader{
		Version: 1,
		Kind: kind,
		ReqId: id,
		ContentLength: uint16(content_length),
		PaddingLength: uint8(-content_length & 7),
	}
}
// this will read ContentLength+PaddingLength bytes, and return the first ContentLength bytes
func (self *fcgiHeader) readContent(r io.Reader) (b []byte, err os.Error) {
	t := int(self.ContentLength) + int(self.PaddingLength)
	b = make([]byte, t)
	if t == 0 {
		return b, nil
	}
	n, err := r.Read(b)
	if n < int(self.ContentLength) {
		return b[0:n], os.NewError(fmt.Sprint("Short read got ", n, "of", t))
	} else if n < t {
		// so we read the content but not the padding, which we _must_ read
		pad := make([]byte, self.PaddingLength)
		if m, err := r.Read(pad); err == nil {
			if m < int(self.PaddingLength) {
				return b[0:n], os.NewError(fmt.Sprint("Short read got ", n, "of", t, " and only", m, "of", self.PaddingLength, "padding"))
			}
		} else {
			return b[0:n], os.NewError(fmt.Sprint("Failed to read padding:", err))
		}
	}
	// Log("readContent{",self.ContentLength,self.PaddingLength,"}: ",b[0:self.ContentLength],b[self.ContentLength:n])
	// discard the padding bytes from the final selection
	return b[0:self.ContentLength], err
}
func (self *fcgiHeader) writePadding(w io.Writer) os.Error {
	if self.PaddingLength > 0 {
		pad := make([]byte, self.PaddingLength) // TODO: less allocation, since we throw it away anyway...
		Log("fcgiWrite: Pad", pad)
		_, err := w.Write(pad)
		return err
	}
	return nil
}

// fcgiWrite() writes a single FastCGI record to a Writer
func fcgiWrite(conn io.Writer, kind uint8, reqId uint16, b []byte) (n int, err os.Error) {
	h := newFcgiHeader(kind, reqId, len(b))
	Log("fcgiWrite: Header", conn, h)
	writeStruct(conn, h)
	if len(b) > 0 {
		n, err = conn.Write(b)
		Log("fcgiWrite: Body", b)
		h.writePadding(conn)
	}
	return n, err
}

// FastCGI has its own pair encoding: <name-len><val-len><name><val>, with a couple kinks.
// but these two functions read a chunk at a time, see parseFcgiParams for usage
func getOneSize(slice []byte) (uint32, []byte) {
	size := slice[0]
	r := 1
	if size>>7 == 1 {
		er := binary.Read(bytes.NewBuffer(slice[0:4]), binary.BigEndian, &size)
		if er != nil {
			Log("binary.Read err:", er)
			return 0, slice[len(slice):len(slice)]
		}
		r = 4
	}
	return uint32(size), slice[r:len(slice)]
}
func getOneValue(slice []byte, size uint32) (string, []byte) {
	if int(size) > len(slice) {
		Log("invalid pair encoding", slice, size)
		return "", slice[0:0]
	}
	return string(slice[0:size]), slice[size:len(slice)]
}

// Handler returns an http.Handler that will dispatch requests to FastCGI Responders
func Handler(addrs []string) http.Handler {
	// first, connect to all the addrs
	responders := make([]*wsConn, len(addrs))
	e := 0
	for i, addr := range addrs {
		var err os.Error
		if responders[i], err = fcgiDial(addr); err != nil {
			Log("Handler: failed to connect to responder: %#v", err)
			e += 1
		} else {
			Log("Handler: connected to responder %+v", addr)
		}
	}
	// collapse the list of responders, removing connection errors
	if e > 0 {
		if e == len(responders) {
			return nil
		}
		tmp := make([]*wsConn, len(responders)-e)
		j := 0
		for i, _ := range responders {
			if responders[i] != nil {
				tmp[j] = responders[i]
				j += 1
			}
		}
		responders = tmp
	}
	if len(responders) == 0 {
		return nil
	}

	// define an iterator for the responders
	// (round-robin for now)
	nextId := -1 // -1 so the iterator yields 0 first
	getNextResponder := func() *wsConn {
		nextId = (nextId + 1) % len(responders)
		return responders[nextId]
	}

	// then, define the handler
	handler := http.HandlerFunc(func(conn *http.Conn, req *http.Request) {
		responder := getNextResponder()
		if responder != nil {
			reqid := responder.WriteRequest(conn, req)
			// read the response (blocking)
			Log("Handler: reading Response", reqid)
			if response, err := responder.ReadResponse(reqid, req.Method); err == nil {
				// once it is ready, write it out to the real connection
				Log("Handler: writing Response", reqid, response)
				for k, v := range response.Header {
					Log("Handler: setting header ", k, v)
					conn.SetHeader(k, v)
				}
				Log("Handler: sending response status ", response.StatusCode)
				conn.WriteHeader(response.StatusCode)
				if b, err := ioutil.ReadAll(response.Body); err == nil {
					conn.Write(b)
				}
			} else {
				Log("Handler: Failed to read response: ", err)
				conn.WriteHeader(http.StatusInternalServerError)
			}
		} else {
			// TODO: queue it up
			conn.WriteHeader(http.StatusServiceUnavailable)
		}
	})

	return handler
}

// fcgiRequest holds the state for an in-progress request,
// and provides functions for converting back and forth between fcgiRequest and http.Request
type fcgiRequest struct {
	reqId     uint16
	params    map[string]string
	stdin     *bytes.Buffer
	data      *bytes.Buffer
	close     bool
	startTime int64
	conn      Socket
}

func newFcgiRequest(reqid uint16, conn Socket, flags uint8) *fcgiRequest {
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

// in order to Accept() new connections from an fd, we need some wrappers
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

func (f FDAddr) Network() string { return "fd" }
func (f FDAddr) String() string  { return "fd://" + strconv.Itoa(f.Fd) }

// FDListener is a net.Listener that uses syscall.Accept(fd)
// to accept new connections directly on an already open fd.
// This is needed by FCGI because if we are dynamically spawned,
// the webserver will open the socket for us.
type FDListener struct {
	fd int
}

// ListenFD creates an FDListener, which listens on the given fd.
// fd must refer to an already open socket
func ListenFD(fd int) (*FDListener, os.Error) { return &FDListener{fd: fd}, nil }
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

// FcgiListener is a net.Listener that you can pass to http.Serve(),
// http.Serve() will then, in effect, be running a FastCGI Responder
type FcgiListener struct {
	net.Listener
	c   chan *rsConn
	err chan os.Error
}

// Listen() creates a new FcgiListener
func Listen(listenAddress string) (*FcgiListener, os.Error) {
	if l, err := net.Listen("tcp", listenAddress); err == nil {
		self := &FcgiListener{
			Listener: l,
			c: make(chan *rsConn),
			err: make(chan os.Error, 1),
		}
		// start a producer that calls Accept on the real listener
		go func() {
			for {
				if self == nil || self.Listener == nil {
					break
				}
				if c, err := self.Listener.Accept(); err == nil {
					go self.readAllPackets(c) // once enough packets have been read, FcgiListener.Accept() will yield a connection
				} else {
					self.err <- err
					break
				}
			}
		}()
		return self, nil
	} else {
		return nil, err
	}
	return nil, nil
}
// readAllPackets is the goroutine that will read FCGI records off the real socket
// and dispatch the rsConns to Accept() when they are ready
func (self *FcgiListener) readAllPackets(conn Socket) {
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
func (self *FcgiListener) Accept() (net.Conn, os.Error) {
	select {
	case c := <-self.c:
		Log("Listener: Accept() releasing connection", c)
		return net.Conn(c), nil
	case err := <-self.err:
		return nil, err
	}
	return nil, nil
}

// ListenAndServe is a quick helper for calling Listen() and then http.Serve
func ListenAndServe(addr string, handler http.Handler) (err os.Error) {
	if flisten, err := Listen(addr); err == nil {
		http.Serve(flisten, handler)
	}
	return err
}

// ListenAndServeFD is a quick helper for calling ListenFD() and http.Serve
func ListenAndServeFD(fd int, handler http.Handler) (err os.Error) {
	if flisten, err := ListenFD(fd); err == nil {
		http.Serve(flisten, handler)
	}
	return err
}

// rsConn is the responder-side of a connection to the webserver, it looks like a net.Conn
// It is created by calls to FcgiListener.Accept() from inside http.Serve().
// It won't be created until a complete request has been buffered off a real socket.
// Read() here will read from that buffer only, never a real socket.
//  - Its possible that in the future calls to Read() would block waiting for chunks of FCGI_STDIN,etc to arrive
// Write() will send FCGI_STDOUT records back to the web server.
type rsConn struct {
	reqId      uint16        // the request id to put in the headers of the output packets
	conn       Socket        // the ReadWriteCloser to do real I/O on
	buf        *bytes.Buffer // the buffer that .Read() will read from
	remoteAddr net.Addr      // this should be the address of the far remote, the user's IP at their browser
	localAddr  net.Addr      // this is undefined, is it the webserver or the responder? but its required
	close      bool          // whether, when closed, to also close conn
	closedOut  bool          // did the EOF STDOUT messages get sent already?
	closedErr  bool          // did the EOF STDERR messages get sent already?
}

func newRsConn(conn Socket, req *fcgiRequest) *rsConn {
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
// the http server always closes the connection right now
// so we can use that to "close" the fcgi request (not the socket unless .close)
// once the http server supports keep-alive connections,
// this assumption will no longer hold, then http.Conn will need to expose its endRequest or something
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

// wsConn is the webserver-side of a connection to a FastCGI Responder
type wsConn struct {
	addr    string
	conn    Socket
	buffers []*closeBuffer // (a closable bytes.Buffer)
	signals []chan bool    // used to signal ReadResponse
	nextId  uint16
}

// fcgiDial connects to a FastCGI Responder over TCP, and returns the wsConn for the connection
func fcgiDial(addr string) (self *wsConn, err os.Error) {
	if conn, err := dialAddr(addr); err == nil {
		self = &wsConn{
			addr: addr,
			conn: conn,
			buffers: make([]*closeBuffer, 65535),
			signals: make([]chan bool, 65535),
			nextId: 1,
		}
		for i, _ := range self.signals {
			self.signals[i] = make(chan bool, 1) // if the request completes before the ReadResponse, it shouldnt block
		}
		// start the goroutine that will read all the response packets and assemble them
		go self.readAllPackets()
	}
	return self, err
}
// fcgiWrite sends out FastCGI records on this connection
func (self *wsConn) fcgiWrite(kind uint8, id uint16, data []byte) (n int, err os.Error) {
	return fcgiWrite(self.conn, kind, id, data)
}
// readAllPackets is a goroutine that reads everything from the connection
// and dispatches responses when they are complete (FCGI_END_REQUEST) is recieved
func (self *wsConn) readAllPackets() {
	h := &fcgiHeader{}
	for {
		h.Version = 0
		err := readStruct(self.conn, h)
		switch {
		case err == os.EOF:
			Log("wsConn: EOF")
			goto close
		case err != nil:
			Log("wsConn: error reading FcgiHeader:", err)
			goto close
		case h.Version != 1:
			Log("wsConn: read a header with invalid version", h.Version, h)
			goto close
		}
		if req := self.buffers[h.ReqId]; req == nil {
			Log("wsConn: got a response with unknown request id", h.ReqId, h)
			continue
		} else {
			switch h.Kind {
			case FCGI_STDOUT:
				if content, err := h.readContent(self.conn); err == nil {
					Log("wsConn: got STDOUT:", strconv.Quote(string(content)))
					if len(content) > 0 {
						req.Write(content)
					} else {
						req.Close()
					}
				}
			case FCGI_STDERR:
				if content, err := h.readContent(self.conn); err == nil {
					Log("wsConn: got STDERR:", strconv.Quote(string(content)))
					if len(content) > 0 {
						req.WriteString("Error: ")
						req.Write(content)
						req.WriteString("\r\n")
					}
				}
			case FCGI_END_REQUEST:
				Log("wsConn: got END_REQUEST")
				e := new(fcgiEndRequest)
				readStruct(self.conn, e)
				Log("wsConn: appStatus ", e.AppStatus, " protocolStatus ", e.ProtocolStatus)
				buf := self.buffers[h.ReqId]
				switch e.ProtocolStatus {
				case FCGI_REQUEST_COMPLETE:
					// buf has been filled already by calls to .Write from inside some other Handler
				case FCGI_CANT_MPX_CONN:
					buf.Reset()
					buf.WriteString("HTTP/1.1 500 Internal Server Error\r\n\r\nFastCGI Responder says it cannot multiplex connections.\r\n")
				case FCGI_OVERLOADED:
					buf.Reset()
					buf.WriteString("HTTP/1.1 503 Service Unavailable\r\n\r\nFastCGI Responder says it is overloaded.\r\n")
				case FCGI_UNKNOWN_ROLE:
					buf.Reset()
					buf.WriteString("HTTP/1.1 500 Internal Server Error\r\n\r\nFastCGI Responder has been asked for an unknown role.\r\n")
				}
				self.signals[h.ReqId] <- true
				// dont free the request id yet, because it might not have been read yet
			default:
				Log("wsConn: responder sent unknown packet type:", h.Kind, h)
			}
		}
	}
close:
	self.conn.Close()
}
func (self *wsConn) Close() os.Error { return self.conn.Close() }
func (self *wsConn) getNextReqId() (reqid uint16) {
	start := self.nextId
	for self.buffers[self.nextId] != nil {
		self.nextId = (self.nextId + 1) % uint16(len(self.buffers))
		if self.nextId == 0 {
			self.nextId = 1
		}
		if self.nextId == start {
			return 0
		}
	}
	return self.nextId
}
func (self *wsConn) freeReqId(reqid uint16) {
	self.buffers[reqid] = nil
	self.nextId = reqid
}
func (self *wsConn) encodeSize(size int) []byte {
	if size > 127 {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, uint32(size))
		return buf
	}
	buf := make([]byte, 1)
	buf[0] = uint8(size)
	return buf
}
// WriteRequest takes the real http.Conn and http.Request objects
// (passed in by the FcgiHandler running on the http server)
// it sends the request data to the responder, and reports the request id
// request ids are small, quickly reused, and relative to this responder
func (self *wsConn) WriteRequest(con *http.Conn, req *http.Request) (reqid uint16) {
	reqid = self.getNextReqId()
	freq := getFcgiRequest(reqid, con, req)
	self.buffers[reqid] = new(closeBuffer)

	Log("wsConn: sending BEGIN_REQUEST")
	/* Send a FCGI_BEGIN_REQUEST */
	writeStruct(self.conn, newFcgiHeader(FCGI_BEGIN_REQUEST, reqid, 8))
	// default to keeping the fcgi connection open
	flags := uint8(FCGI_KEEP_CONN)
	// unless requested otherwise by the http.Request
	if req.Close {
		Log("wsConn: setting fcgiRequest to close the fcgi connection, is this right?")
		flags = 0
	}
	writeStruct(self.conn, fcgiBeginRequest{
		Role: FCGI_RESPONDER,
		Flags: flags,
	})

	/* Encode and Send the FCGI_PARAMS */
	buf := bytes.NewBuffer(make([]byte, 0, FCGI_MAX_WRITE))
	for k, v := range freq.params {
		buf.Write(self.encodeSize(len(k)))
		buf.Write(self.encodeSize(len(v)))
		buf.WriteString(k)
		buf.WriteString(v)
	}
	Log("wsConn: sending FCGI_PARAMS")
	self.fcgiWrite(FCGI_PARAMS, reqid, buf.Bytes())

	buf2 := make([]byte, 0, FCGI_MAX_WRITE)
	/* Now write the FCGI_STDIN, read from the Body of the request */
	Log("wsConn: sending FCGI_STDIN")
	// get the stdin data from the fcgiRequest
	for freq.stdin != nil && freq.stdin.Len() > 0 {
		n, err := freq.stdin.Read(buf2[0:])
		if n == 0 || err == os.EOF {
			break
		} else if err != nil {
			Log("wsConn: Error reading fcgiRequest.stdin:", err)
			break
		} else {
			Log("wsConn: sending FCGI_STDIN")
			self.fcgiWrite(FCGI_STDIN, reqid, buf2[0:n])
		}
	}
	// write the last FCGI_STDIN
	self.fcgiWrite(FCGI_STDIN, reqid, []byte{})

	/* Send the FCGI_DATA */
	Log("wsConn: sending FCGI_DATA")
	for freq.data != nil && freq.data.Len() > 0 {
		n, err := freq.data.Read(buf2[0:])
		if n == 0 || err == os.EOF {
			break
		} else if err != nil {
			Log("Error reading fcgiRequest.data:", err)
			break
		} else {
			Log("wsConn: sending FCGI_DATA")
			self.fcgiWrite(FCGI_DATA, reqid, buf2[0:n])
		}
	}
	// write the last FCGI_DATA
	self.fcgiWrite(FCGI_DATA, reqid, []byte{})

	return reqid
}
func (self *wsConn) ReadResponse(reqid uint16, method string) (ret *http.Response, err os.Error) {
	// this blocks until END_REQUEST is recieved from the responder
	<-self.signals[reqid] // wait for this reqid to be finished
	Log("wsConn: ReadResponse ready for reqid", reqid)
	ret, err = http.ReadResponse(bufio.NewReader(self.buffers[reqid]), method)
	self.freeReqId(reqid)
	return ret, err
}

type closeBuffer struct {
	bytes.Buffer
	closed bool
}

func (self *closeBuffer) Reset() {
	self.closed = false
	self.Buffer.Reset()
}
func (self *closeBuffer) Close() os.Error {
	self.closed = true
	return nil
}
func (self *closeBuffer) Write(p []byte) (n int, err os.Error) {
	if self.closed {
		return 0, os.EOF
	}
	return self.Buffer.Write(p)
}

/* Utility Code: */

// when the webserver sends us "ACCEPT_ENCODING" as a header,
// (in the FCGI_PARAMS) standardize it like: Accept-Encoding
func standardCase(str []byte) string {
	ret := make([]byte, len(str))
	first := true
	for i := 0; i < len(str); i++ {
		if str[i] == '_' {
			ret[i] = '-'
			first = true
		} else if first {
			ret[i] = str[i]
			first = false
		} else {
			ret[i] = strings.Bytes(strings.ToLower(string(str[i])))[0]
		}
	}
	return string(ret)
}

// write a struct in binary encoding to a writer
func writeStruct(w io.Writer, data interface{}) os.Error {
	err := binary.Write(w, binary.BigEndian, data)
	if err != nil && err != os.EOF {
		Log("WriteTo Error:", err)
	}
	return err
}

// read a struct in binary encoding from a reader
func readStruct(r io.Reader, data interface{}) (err os.Error) {
	err = binary.Read(r, binary.BigEndian, data)
	if err != nil && err != os.EOF {
		Log("ReadFrom Error:", err)
	}
	return err
}


// a reader that supports a dummy Close()
type nopCloser struct {
	io.Reader
}

func (nopCloser) Close() os.Error { return nil }

// a quick wrapper for DialTCP that handles name resolution
func dialAddr(addr string) (conn *net.TCPConn, err os.Error) {
	laddr, _ := net.ResolveTCPAddr("127.0.0.1:0")
	if raddr, err := net.ResolveTCPAddr(addr); err == nil {
		if sock, err := net.DialTCP("tcp4", laddr, raddr); err == nil {
			conn = sock
		}
	}
	return conn, err
}

// copied from http.request
// this does the atoi conversion at different offsets i in s
func atoi(s string, i int) (n, i1 int, ok bool) {
	const Big = 1000000
	if i >= len(s) || s[i] < '0' || s[i] > '9' {
		return 0, 0, false
	}
	n = 0
	for ; i < len(s) && '0' <= s[i] && s[i] <= '9'; i++ {
		n = n*10 + int(s[i]-'0')
		if n > Big {
			return 0, 0, false
		}
	}
	return n, i, true
}
