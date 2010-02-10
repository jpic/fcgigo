// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Defines the FastCGI http.Handler

package fcgi

import (
	"os"
	"io"
	"io/ioutil"
	"bufio"
	"bytes"
	"strings"
	"strconv"
	"net"
	"http"
	"fmt"
	"syscall"
	"path"
	"runtime"
)

type handler struct {
	http.Handler
	responders chan *wsConn
}

// Handler returns an http.Handler that will dispatch requests to FastCGI Responders
// addrs are a list of addresses that include a prefix for what kind of network they are on
// e.g. tcp://127.0.0.1:1234, unix:///tmp/some.sock, exec:///usr/bin/php
func Handler(addrs []string) (http.Handler, os.Error) {
	self := &handler{
		responders: make(chan *wsConn, len(addrs)), // make room to buffer all of the responders if we are idle
	}
	// first, connect to all the addrs, and load the responders in the channel
	e := 0 // number of connection errors
	for _, addr := range addrs {
		s := strings.Split(addr, "://", 0)
		if len(s) != 2 {
			return nil, os.NewError(fmt.Sprint("Invalid address given to fcgi.Handler()", addr))
		}
		switch strings.ToLower(s[0]) {
		case "tcp":
			conn, err := net.Dial("tcp", "", s[1])
			if err != nil {
				Log("Handler: failed to connect to tcp responder:", s[1], err)
				e += 1
				continue
			}
			Log("Handler: connected to TCP responder.", s[1])
			self.responders <- newWsConn(conn, s[1])
		case "unix":
			conn, err := net.Dial("unix", "", s[1])
			if err != nil {
				Log("Handler: failed to connect to responder on unix socket:", s[1], err)
				e += 1
				continue
			}
			Log("Handler: connected to UNIX responder.", s[1])
			self.responders <- newWsConn(conn, s[1])
		case "exec":
			conn, err := fcgiDialExec(s[1])
			if err != nil {
				Log("Handler: failed to exec fcgi responder program:", s[1], err)
				e += 1
				continue
			}
			Log("Handler: connected to EXEC responder.", s[1])
			self.responders <- conn
		default:
			return nil, os.NewError(fmt.Sprint("Invalid responder address", addr, "with unknown protocol", s[0]))
		}
	}
	if e == len(addrs) {
		return nil, os.NewError("No FastCGIResponders connected.")
	}
	return self, nil
}

func (self *handler) ServeHTTP(conn *http.Conn, req *http.Request) {
	var response *http.Response
	var err os.Error
	var body []byte
	// get the next available responder
	responder := <-self.responders
	// a multiplexing responder is available again immediately
	if responder.multiplex {
		self.responders <- responder
	}
	// send the request to the FastCGI responder
	reqid := responder.WriteRequest(conn, req)
	// read the response (blocking)
	if response, err = responder.ReadResponse(reqid, req.Method); err != nil {
		Log("Handler: Failed to read response:", err)
		conn.WriteHeader(http.StatusInternalServerError)
		io.WriteString(conn, err.String())
		return
	}
	// a non-multiplexing responder has to wait until the response is fully read before it is available
	if !responder.multiplex {
		self.responders <- responder
	}
	// once response is ready, write it out to the real connection
	for k, v := range response.Header {
		conn.SetHeader(k, v)
	}
	conn.WriteHeader(response.StatusCode)
	if body, err = ioutil.ReadAll(response.Body); err != nil {
		Log("Handler: Error reading response.Body:", err)
		io.WriteString(conn, err.String())
		return
	}
	conn.Write(body)
}


// wsConn is the webserver-side of a connection to a FastCGI Responder
type wsConn struct {
	addr      string
	pid       int // only meaningful when addr is exec:...
	conn      net.Conn
	buffers   []*closeBuffer // a closable bytes.Buffer
	signals   []chan bool    // used to signal ReadResponse
	nextId    uint16         // the reqId of the next request on this connection
	multiplex bool           // does the responder on the other side support multiplex?
}

func newWsConn(conn net.Conn, addr string) *wsConn {
	if conn == nil {
		return nil
	}
	self := &wsConn{
		addr: addr,
		conn: conn,
		buffers: make([]*closeBuffer, 256),
		signals: make([]chan bool, 256),
		nextId: 1,
		multiplex: true,
	}
	for i, _ := range self.signals {
		self.signals[i] = make(chan bool, 1) // if the request completes before the ReadResponse, it shouldnt block
	}
	// start the goroutine that will read all the response packets and assemble them
	go self.readAllPackets()
	return self
}

// fcgiDialExec will ForkExec a new process, and returns a wsConn that connects to its stdin
func fcgiDialExec(binpath string) (self *wsConn, err os.Error) {
	listenBacklog := 1024
	dir, file := path.Split(binpath)
	socketPath := "/tmp/" + file + ".sock"
	socketIndex := 0 // if the first socket is really in use (we are launching this same process more than once)
	// then the socketIndex will start incrementing so we assign .sock-1 to the second process, etc
	for {
		err := os.Remove(socketPath)
		// if the socket file is stale, but not in use, the Remove succeeds with no error
		if err == nil {
			goto haveSocket // success, found a stale socket we can re-use
		}
		// otherwise we have to check what the error was
		switch err.String() {
		case "remove " + socketPath + ": no such file or directory":
			goto haveSocket // success, we have found an unused socket.
		default:
			// if its really in use, we start incrementing socketIndex
			socketIndex += 1
			socketPath = "/tmp/" + file + ".sock-" + strconv.Itoa(socketIndex)
			Log("Socket was in use, trying:", socketPath)
		}
	}
haveSocket:
	Log("Using socketPath:", socketPath)
	var fd, pid, cfd, errno int
	var sa syscall.Sockaddr
	// we can almost use UnixListener to do this except it doesn't expose the fd it's listening on
	// create a new socket
	if fd, errno = syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0); errno != 0 {
		Log("Creating first new socket failed:", syscall.Errstr(errno))
		return nil, os.NewError(fmt.Sprint("Creating first new socket failed:", syscall.Errstr(errno)))
	}
	// bind the new socket to socketPath
	if errno = syscall.Bind(fd, &syscall.SockaddrUnix{Name: socketPath}); errno != 0 {
		Log("Bind failed:", syscall.Errstr(errno))
		return nil, os.NewError(fmt.Sprint("Bind failed:", syscall.Errstr(errno)))
	}
	// start to listen on that socket
	if errno = syscall.Listen(fd, listenBacklog); errno != 0 {
		Log("Listen failed:", syscall.Errstr(errno))
		return nil, os.NewError(fmt.Sprint("Listen failed:", syscall.Errstr(errno)))
	}
	// then ForkExec a new process, and give this listening socket to them as stdin
	if pid, errno = syscall.ForkExec(file, []string{}, []string{}, dir, []int{fd}); errno != 0 {
		Log("ForkExec failed:", syscall.Errstr(errno))
		return nil, os.NewError(fmt.Sprint("ForkExec failed:", syscall.Errstr(errno)))
	}
	// now create a socket for the client-side of the connection
	if cfd, errno = syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0); errno != 0 {
		Log("Creating new socket on webserver failed:", syscall.Errstr(errno))
		return nil, os.NewError(fmt.Sprint("Creating new socket on webserver failed:", syscall.Errstr(errno)))
	}
	// find the address of the socket we gave the new process
	if sa, errno = syscall.Getsockname(fd); errno != 0 {
		Log("Getsockname failed:", syscall.Errstr(errno))
		return nil, os.NewError(fmt.Sprint("Getsockname failed:", syscall.Errstr(errno)))
	}
	// connect our client side to the remote address
	if errno = syscall.Connect(cfd, sa); errno != 0 {
		Log("Connect failed:", syscall.Errstr(errno))
		return nil, os.NewError(fmt.Sprint("Connect failed:", syscall.Errstr(errno)))
	}
	// return a wrapper around the client side of this connection
	ws := newWsConn(fileConn{os.NewFile(cfd, "exec://"+binpath)}, "exec://"+binpath)
	ws.pid = pid
	runtime.SetFinalizer(ws, finalizer)
	return ws, nil
}

// finalizer will be passed to runtime.SetFinalizer, and be used to kill the child process this connection created
// if we miss this, the child checks periodically to see if we died, so it will go down eventually on its own
func finalizer(self *wsConn) {
	Log("wsConn: finalizer")
	if self.pid > 0 {
		// send the process a SIGTERM
		Log("wsConn: sending child proc a SIGTERM")
		syscall.Syscall(syscall.SYS_KILL, uintptr(self.pid), syscall.SIGTERM, 0)
		// if _, err := os.Wait(self.pid, 0); err != nil {
		// syscall.Syscall(syscall.SYS_KILL, uintptr(self.pid), syscall.SIGKILL, 0)
		// return
		// }
	}
}

// String() returns a descriptive string about this connection
func (self *wsConn) String() string {
	if self == nil {
		return "nil"
	}
	return "{wsConn@" + self.addr + "}"
}

// fcgiWrite sends out FastCGI records on this connection
func (self *wsConn) fcgiWrite(kind uint8, id uint16, data []byte) (n int, err os.Error) {
	return fcgiWrite(self.conn, kind, id, data)
}

// readAllPackets is a goroutine that reads everything from the connection
// and dispatches responses when they are complete (fcgiEndRequest is recieved).
func (self *wsConn) readAllPackets() {
	h := &header{}
	for {
		h.Version = 0
		err := readStruct(self.conn, h)

		// check errors
		switch {
		case err == os.EOF:
			goto close
		case err != nil:
			Log("wsConn: error reading FcgiHeader:", err)
			goto close
		case h.Version != 1:
			Log("wsConn: read a header with invalid version", h.Version, h)
			goto close
		}

		// get the request the packet refers to
		req := self.buffers[h.ReqId]
		if req == nil {
			Log("wsConn: got a response with unknown request id", h.ReqId, h)
			continue
		}

		// check the packet type
		switch h.Kind {
		case fcgiStdout:
			if content, err := h.readContent(self.conn); err == nil {
				if len(content) > 0 {
					req.Write(content)
				} else {
					req.Close()
				}
			}
		case fcgiStderr:
			if content, err := h.readContent(self.conn); err == nil {
				if len(content) > 0 {
					req.WriteString("Error: ")
					req.Write(content)
					req.WriteString("\r\n")
				}
			}
		case fcgiEndRequest:
			Log("wsConn: got END_REQUEST", h.ReqId)
			readEndRequest(self.conn, req)
			self.signals[h.ReqId] <- true
			// dont free the request id yet, because it might not have been read yet
		default:
			Log("wsConn: responder sent unknown packet type:", h.Kind, h)
		}
	}
close:
	self.Close()
}

func readEndRequest(conn net.Conn, req *closeBuffer) {
	e := new(endRequest)
	readStruct(conn, e)
	switch e.ProtocolStatus {
	case fcgiRequestComplete:
		// buf has been filled already by calls to .Write from inside some other Handler
	case fcgiCantMpxConn:
		req.Reset()
		req.WriteString("HTTP/1.1 500 Internal Server Error\r\n\r\nFastCGI Responder says it cannot multiplex connections.\r\n")
	case fcgiOverloaded:
		req.Reset()
		req.WriteString("HTTP/1.1 503 Service Unavailable\r\n\r\nFastCGI Responder says it is overloaded.\r\n")
	case fcgiUnknownRole:
		req.Reset()
		req.WriteString("HTTP/1.1 500 Internal Server Error\r\n\r\nFastCGI Responder has been asked for an unknown role.\r\n")
	}
}

// Close closes the underlying connection to the FastCGI responder.
func (self *wsConn) Close() os.Error {
	Log("wsConn: Close()")
	return self.conn.Close()
}

// getNextReqId is an iterator that produces un-used request ids.
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

// freeReqId marks a reqId as usable for another request on this connection.
func (self *wsConn) freeReqId(reqid uint16) {
	self.buffers[reqid] = nil
	self.nextId = reqid
}

// WriteRequest takes the real http.Conn and http.Request objects, and uses them to write a FastCGI request over this wsConn (to the responder on the other side).
// It returns the new request id allocated for use in the exchange.
func (self *wsConn) WriteRequest(con *http.Conn, req *http.Request) (reqid uint16) {
	reqid = self.getNextReqId()
	freq := getRequest(reqid, con, req)
	self.buffers[reqid] = new(closeBuffer)
	Log("wsConn: sending BEGIN_REQUEST", reqid)
	/* Send a fcgiBeginRequest */
	writeStruct(self.conn, newHeader(fcgiBeginRequest, reqid, 8))
	// default to keeping the fcgi connection open
	flags := uint8(fcgiKeepConn)
	// unless requested otherwise by the http.Request
	if req.Close {
		flags = 0
	}
	writeStruct(self.conn, beginRequest{
		Role: fcgiResponder,
		Flags: flags,
	})
	/* Encode and Send the fcgiParams */
	buf := bytes.NewBuffer(make([]byte, 0, fcgiMaxWrite))
	for k, v := range freq.params {
		buf.Write(encodeSize(len(k)))
		buf.Write(encodeSize(len(v)))
		buf.WriteString(k)
		buf.WriteString(v)
	}
	// Log("wsConn: sending fcgiParams")
	self.fcgiWrite(fcgiParams, reqid, buf.Bytes())
	buf2 := make([]byte, 0, fcgiMaxWrite)
	/* Now write the fcgiStdin, read from the Body of the request */
	// Log("wsConn: sending fcgiStdin")
	// get the stdin data from the request
	for freq.stdin != nil && freq.stdin.Len() > 0 {
		n, err := freq.stdin.Read(buf2[0:])
		if n == 0 || err == os.EOF {
			break
		} else if err != nil {
			Log("wsConn: Error reading request.stdin:", err)
			break
		} else {
			// Log("wsConn: sending fcgiStdin")
			self.fcgiWrite(fcgiStdin, reqid, buf2[0:n])
		}
	}
	// write the last fcgiStdin
	self.fcgiWrite(fcgiStdin, reqid, []byte{})
	/* Send the fcgiData */
	// Log("wsConn: sending fcgiData")
	for freq.data != nil && freq.data.Len() > 0 {
		n, err := freq.data.Read(buf2[0:])
		if n == 0 || err == os.EOF {
			break
		} else if err != nil {
			Log("Error reading request.data:", err)
			break
		} else {
			// Log("wsConn: sending fcgiData")
			self.fcgiWrite(fcgiData, reqid, buf2[0:n])
		}
	}
	// write the last fcgiData
	self.fcgiWrite(fcgiData, reqid, []byte{})
	return reqid
}

// ReadResponse waits for a signal that the reqid request is complete.
// It uses http.ReadResponse to read and return an http.Response object from reqid's response buffer.
// After the response is read, the reqid is freed, and might immediately be used again for a new request.
func (self *wsConn) ReadResponse(reqid uint16, method string) (ret *http.Response, err os.Error) {
	<-self.signals[reqid] // wait for this reqid to be finished
	ret, err = http.ReadResponse(bufio.NewReader(self.buffers[reqid]), method)
	self.freeReqId(reqid)
	return ret, err
}

// closeBuffer is a closable buffer, after .Close(), .Write() returns EOF error.
type closeBuffer struct {
	bytes.Buffer
	closed bool
}

// Reset() is the same as Truncate(0) but also re-opens the buffer if it was closed.
func (self *closeBuffer) Reset() {
	self.closed = false
	self.Buffer.Reset()
}

// Close causes any future writes to return EOF.  Reads can continue until the buffer is empty.
func (self *closeBuffer) Close() os.Error {
	self.closed = true
	return nil
}

// Write works like normal except that if the buffer has been closed, it returns (0, os.EOF).
func (self *closeBuffer) Write(p []byte) (n int, err os.Error) {
	if self.closed {
		return 0, os.EOF
	}
	return self.Buffer.Write(p)
}
