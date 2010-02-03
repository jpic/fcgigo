// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
package fcgi

import (
	"os"
	"io"
	"io/ioutil"
	"bufio"
	"bytes"
	"http"
	"strconv"
)


// Handler returns an http.Handler that will dispatch requests to FastCGI Responders
func Handler(addrs []string) http.Handler {
	// first, connect to all the addrs
	responders := make([]*wsConn, len(addrs))
	e := 0
	for i, addr := range addrs {
		var err os.Error
		if responders[i], err = fcgiDial(addr); err != nil {
			Log("Handler: failed to connect to responder:", err)
			e += 1
		} else {
			Log("Handler: connected to responder", addr)
		}
	}
	// collapse the list of responders, removing connection errors
	if e > 0 {
		if e == len(responders) {
			return errorHandler(http.StatusServiceUnavailable, "No FastCGIResponders connected.")
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
		// always try to return a handler
		return errorHandler(http.StatusServiceUnavailable, "No FastCGIResponders connected.")
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

// errorHandler returns an http.Handler that prints an error page
func errorHandler(status int, msg string) http.Handler {
	return http.HandlerFunc(func(c *http.Conn, r *http.Request) {
		c.SetHeader("Content-type", "text/plain")
		c.WriteHeader(status)
		io.WriteString(c, msg)
	})
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
	self.Close()
}
// Close closes the underlying connection to the FastCGI responder
func (self *wsConn) Close() os.Error { return self.conn.Close() }
// getNextReqId is an iterator that produces un-used request ids
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
// freeReqId marks a reqId as usable for another request on this connection
func (self *wsConn) freeReqId(reqid uint16) {
	self.buffers[reqid] = nil
	self.nextId = reqid
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
		buf.Write(encodeSize(len(k)))
		buf.Write(encodeSize(len(v)))
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

// closeBuffer is a closable buffer, after .Close(), .Write() returns EOF error
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
