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
	"net"
	"http"
	"fmt"
	"encoding/binary"
	"syscall"
)

type Dialer interface {
	Dial() (io.ReadWriteCloser, os.Error)
	Addr() net.Addr
}

type dialer struct {
	Dialer
	net  string
	addr string
}

func (self *dialer) Dial() (rwc io.ReadWriteCloser, err os.Error) {
	switch self.net {
	case "tcp":
		fallthrough
	case "unix":
		rwc, err = net.Dial(self.net, "", self.addr)
		if err != nil {
			return nil, err
		}
	case "exec":
		rwc, err = dialExec(self.addr)
		if err != nil {
			return nil, err
		}
	default:
		return nil, os.NewError(fmt.Sprint("Unknown net type:", self.net))
	}
	return rwc, nil
}

type dialerAddr struct {
	net  string
	addr string
}

func (a *dialerAddr) Network() string { return a.net }
func (a *dialerAddr) String() string  { return a.addr }

func (self *dialer) Addr() net.Addr { return &dialerAddr{net: self.net, addr: self.addr} }

func NewDialer(net string, addr string) Dialer {
	return &dialer{net: net, addr: addr}
}

type handler struct {
	http.Handler
	responders chan *wsConn
}

// Handler returns an http.Handler that will dispatch requests to FastCGI Responders
func Handler(dialers []Dialer) (http.Handler, os.Error) {
	if len(dialers) == 0 {
		return nil, os.NewError("You must provide at least one Dialer.")
	}
	self := &handler{
		responders: make(chan *wsConn, len(dialers)),
	}
	e := 0 // # of connection errors
	for _, d := range dialers {
		Log("Dialing:", d)
		r, err := d.Dial()
		if err != nil {
			Log("Error:", err)
			e += 1
			continue
		}
		self.responders <- newWsConn(r, d.Addr(), true)
	}
	if e == len(dialers) {
		return nil, os.NewError("None of the Dialers you provided successfully connected.")
	}
	return self, nil
}

func (self *handler) ServeHTTP(conn *http.Conn, req *http.Request) {
	var response *http.Response
	var err os.Error
	var body []byte
	// get the next available responder
	responder := <-self.responders
	// send the request to the FastCGI responder
	reqid, err := responder.WriteRequest(conn, req)
	if err != nil {
		Log("Handler: Failed to write request to", responder, "disabling (error:", err, ")")
		conn.WriteHeader(http.StatusInternalServerError)
		io.WriteString(conn, err.String())
		responder.Close()
		return
	}
	// a multiplexing responder is available again immediately
	if responder.multiplex {
		self.responders <- responder
	}
	// read the response (blocking)
	if response, err = responder.ReadResponse(reqid, req.Method); err != nil {
		Log("Handler: Failed to read response from", responder, "error:", err)
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
		Log("Handler: Error reading response.Body from", responder, "(error:", err, ")")
		io.WriteString(conn, err.String())
		return
	}
	conn.Write(body)
}

// wsConn is the webserver-side of a connection to a FastCGI Responder
type wsConn struct {
	net       string
	addr      string
	pid       int // only meaningful when addr is exec
	conn      *lockReadWriteCloser
	buffers   []*bytes.Buffer // the buffer for the response data
	signals   []chan bool     // used to signal ReadResponse
	nextId    uint16          // the reqId of the next request on this connection
	multiplex bool            // does the responder on the other side support multiplex?
}

func newWsConn(conn io.ReadWriteCloser, addr net.Addr, multiplex bool) *wsConn {
	if conn == nil {
		return nil
	}
	c := new(lockReadWriteCloser)
	c.ReadWriteCloser = conn
	self := &wsConn{
		net:       addr.Network(),
		addr:      addr.String(),
		conn:      c,
		buffers:   make([]*bytes.Buffer, 4096),
		signals:   make([]chan bool, 4096),
		nextId:    1,
		multiplex: multiplex,
	}
	for i, _ := range self.signals {
		self.signals[i] = make(chan bool, 1) // if the request completes before the ReadResponse, it shouldnt block
		self.buffers[i] = nil
	}
	// start the goroutine that will read all the response packets and assemble them
	go self.readAllPackets()
	return self
}

// String() returns a descriptive string about this connection
func (self *wsConn) String() string {
	if self == nil {
		return "nil"
	}
	return fmt.Sprint("{wsConn@", self.addr, "}")
}

// readAllPackets is a goroutine that reads everything from the connection
// and dispatches responses when they are complete (typeEndRequest is recieved).
func (self *wsConn) readAllPackets() {
	// we only create one header instance, and refill it as packets arrive
	h := new(header)
	for {
		// read the header
		err := binary.Read(self.conn, binary.BigEndian, h)

		// check errors
		switch {
		case err == os.EOF:
			goto disconnected
		case err != nil:
			self.Log("error reading header:", err)
			goto disconnected
		case h.Version != 1:
			self.Log("header has INVALID VERSION", h.Version, h)
			goto disconnected
		}

		// get the request the packet refers to
		buf := self.buffers[h.ReqId]

		// check that the request exists
		if buf == nil {
			self.Log("header has UNKNOWN REQUEST ID", h.ReqId, h)
			continue
		}

		// check the packet type
		switch h.Kind {
		case typeStdout:
			content, err := h.readContent(self.conn)
			if err != nil {
				self.Log("Failed to readContent from typeStdout:", err)
				break
			}
			buf.Write(content)
		case typeStderr:
			content, err := h.readContent(self.conn)
			if err != nil {
				self.Log("Failed to readContent from typeStderr:", err)
			}
			buf.WriteString("Error: ")
			buf.Write(content)
			buf.WriteString("\r\n")
		case typeEndRequest:
			readEndRequest(self.conn, buf)
			// signal to ReadResponse that the buffer is full and ready to read
			self.signals[h.ReqId] <- true
		default:
			self.Log("responder sent unknown packet type:", h.Kind, h)
		}
	}
disconnected:
	self.Close() // just in case it isnt closed already
}

func readEndRequest(conn io.Reader, buf *bytes.Buffer) {
	e := new(endRequest)
	binary.Read(conn, binary.BigEndian, e)
	switch e.ProtocolStatus {
	case statusRequestComplete:
		// buf has been filled already by calls to .Write from inside some other Handler
	case statusCantMultiplex:
		buf.Reset()
		buf.WriteString("HTTP/1.1 500 Internal Server Error\r\n\r\nFastCGI Responder says it cannot multiplex connections.\r\n")
	case statusOverloaded:
		buf.Reset()
		buf.WriteString("HTTP/1.1 503 Service Unavailable\r\n\r\nFastCGI Responder says it is overloaded.\r\n")
	case statusUnknownRole:
		buf.Reset()
		buf.WriteString("HTTP/1.1 500 Internal Server Error\r\n\r\nFastCGI Responder has been asked for an unknown role.\r\n")
	}
}

// getNextReqId is an iterator that produces un-used request ids.
func (self *wsConn) getNextReqId() (reqid uint16) {
	// avoid searching for a free id by keeping track of the most likely next free one
	// freeReqId will set self.nextId as well to help avoid searching
	if self.buffers[self.nextId] == nil {
		self.nextId += 1
		return self.nextId - 1
	}
	// then only search if we have to
	for n := 1; n < len(self.buffers); n++ {
		if self.buffers[n] == nil {
			self.nextId = uint16(n + 1)
			return uint16(n)
		}
	}
	return 0
}

// freeReqId marks a reqId as usable for another request on this connection.
func (self *wsConn) freeReqId(reqid uint16) {
	self.buffers[reqid] = nil
	self.nextId = reqid
}

// WriteRequest takes the real http.Conn and http.Request objects, and uses them to write a FastCGI request over this wsConn (to the responder on the other side).
// It returns the new request id allocated for use in the exchange.
func (self *wsConn) WriteRequest(con *http.Conn, req *http.Request) (reqid uint16, err os.Error) {
	reqid = self.getNextReqId()
	// convert the http.Request to our internal request object, with params in fcgi format
	freq := getRequest(reqid, con, req)
	// allocate the buffer for the response
	self.buffers[reqid] = new(bytes.Buffer)
	// then compute the fcgi flags
	// default to keeping the server<->responder connection open
	flags := uint8(flagKeepConn)
	// send a beginRequest packet
	if _, err = writeBeginRequest(self.conn, reqid, roleResponder, flags); err != nil {
		return 0, err
	}
	// encode the params to a byte slice
	buf := bytes.NewBuffer(make([]byte, 0, maxWrite))
	for k, v := range freq.params {
		buf.Write(encodeSize(len(k)))
		buf.Write(encodeSize(len(v)))
		buf.WriteString(k)
		buf.WriteString(v)
	}
	// then write the typeParams record
	if err = writeRecord(self.conn, typeParams, reqid, buf.Bytes()); err != nil {
		return 0, err
	}
	// read blocks from the requests stdin, and send typeStdin records
	buf2 := make([]byte, 0, maxWrite)
	for freq.stdin != nil && freq.stdin.Len() > 0 {
		n, err := freq.stdin.Read(buf2[0:])
		if n == 0 || err == os.EOF {
			break
		} else if err != nil {
			self.Log("Error reading request.stdin:", err)
			break
		} else {
			if err = writeRecord(self.conn, typeStdin, reqid, buf2[0:n]); err != nil {
				return 0, err
			}
		}
	}
	// write the typeStdin close message
	if err = writeRecord(self.conn, typeStdin, reqid, []byte{}); err != nil {
		return 0, err
	}
	// read blocks from the requests data stream, and send typeData records
	for freq.data != nil && freq.data.Len() > 0 {
		n, err := freq.data.Read(buf2[0:])
		if n == 0 || err == os.EOF {
			break
		} else if err != nil {
			self.Log("Error reading request.data:", err)
			break
		} else {
			if err = writeRecord(self.conn, typeData, reqid, buf2[0:n]); err != nil {
				return 0, err
			}
		}
	}
	// write the typeData close message
	if err = writeRecord(self.conn, typeData, reqid, []byte{}); err != nil {
		return 0, err
	}
	return reqid, nil
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

// Close ends our connection with the responder.  If the responder was an "exec"
// responder, then we kill the process (since there is no way to reconnect to it).
func (self *wsConn) Close() os.Error {
	if self.net == "exec" && self.pid > 0 {
		self.Log("closing 'exec' child process...")
		syscall.Syscall(syscall.SYS_KILL, uintptr(self.pid), syscall.SIGTERM, 0)
	}
	// wake up any blocked ReadResponse calls (which will fail when awoken early)
	for i := 0; i < len(self.signals); i++ {
		self.signals[i] <- true
	}
	return self.conn.Close()
}

func (self *wsConn) Log(msg string, v ...) {
	msg = fmt.Sprintf("wsConn(%s): %s", self.net, msg)
	Log(msg, v)
}
