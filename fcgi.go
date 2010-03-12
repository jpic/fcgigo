// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
This package implements FastCGI for use with http.Serve().  It provides
two main pieces: fcgi.Handler and fcgi.Listener.

fcgi.Handler returns a http.Handler that will do round-robin dispatch
to remote FastCGI Responders to handle each request. You create one by
calling fcgi.Handler().

fcgi.Listener is a net.Listener that you can pass to http.Serve()
to produce a FastCGI Responder. You get one by calling fcgi.Listen().
See example and tests for details.

Example: You want a create a FastCGI responder that a webserver can
connect to via a TCP socket.

	package main

	import (
		"http"
		"http/fcgi"
	)

	func HelloServer(con *http.Conn, req *http.Request) {
		io.WriteString(con, "hello, world!\n")
	}

	func main() {
		http.Handle("/hello", http.HandlerFunc(HelloServer))
		listener, err := fcgi.Listen("tcp", "127.0.0.1:7134")
		if err != nil {
			return
		}
		http.Serve(listener, nil)
	}

Now, if you configured lighttpd to connect to:
	( "host" => "127.0.0.1",
	  "port" => 7134 )

It would serve your hello, world application.

Example: You want to create an application that can be spawned by the
webserver to service FastCGI requests.

	package main

	import (
		"http"
		"http/fcgi"
	)

	func main() {
		listener, err := fcgi.Listen("exec", "")
		if err != nil {
			return
		}
		http.Serve(listen, nil)
	}

Now, you would configure lighttpd to use:
	( "bin-path" => "<path_to_your_executable>")

Example: You have an existing FastCGI application running on a TCP host
and want Go's http.Serve to send requests to it for servicing.

	handler, err := fcgi.Handler([]string{
		"tcp://127.0.0.1:7134",
		"tcp://127.0.0.1:7135",
		// ... repeat for each responder ...
	})
	http.Handle("/", handler)
	http.ListenAndServe(":80", nil)

Example: You want to serve static files, or errors, or anything,
immediately, while sending only some other requests to a FastCGI
Responder.

	handler, err := fcgi.Handler([]string{
		// ... as above ...
	})
	http.Handle("/myapp", handler)
	http.Handle("/static", http.FileServer(...))
	http.ListenAndServe(":80", nil)

In this example, the http server will use the FileServer to serve files
from /static locally, but all other requests will be sent along to
the Responder.

See fcgi_test.go for examples of all the Listener and Handler types;
also, how to use both a Handler and Listener in the same process.
(hint: they cannot share the same default muxer).

*/
package fcgi

// fcgi.go defines the raw protocol and some utils
// listener.go defines fcgi.Listener, et al
// handler.go defines fcgi.Handler, et al

import (
	"log"
	"os"
	"io"
	"encoding/binary"
	"bytes"
	"fmt"
	"sync"
)

// Log is the logging function used throughout this package.
// By default, it will log nothing.  But, if you set this to log.Stderr, or some other logger that you create, it will use that as well.
var Log = dontLog

func dontLog(k string, v ...) {}

var doLog = log.Stderr

const (
	// The fd to use when we are execed by the web-server
	listenSockFileNo = iota

	// Packet Types (header.Kind)
	typeBeginRequest
	typeAbortRequest
	typeEndRequest
	typeParams
	typeStdin
	typeStdout
	typeStderr
	typeData
	typeGetValues
	typeGetValuesResult
	typeUnknownType
	typeMaxType = typeUnknownType
)

// Keep the connection between web-server and responder open after request
const flagKeepConn = 1

// Max amount of data in a FastCGI record body
const maxWrite = 65535

// Roles (beginRequest.Roles)
const (
	roleResponder = iota + 1 // only Responders are implemented.
	roleAuthorizer
	roleFilter
)

// ProtocolStatus (in endRequest)
const (
	statusRequestComplete = iota
	statusCantMultiplex
	statusOverloaded
	statusUnknownRole
)

type header struct {
	Version       uint8
	Kind          uint8
	ReqId         uint16
	ContentLength uint16
	PaddingLength uint8
	Reserved      uint8
}
type endRequest struct {
	AppStatus      uint32
	ProtocolStatus uint8
	Reserved       [3]uint8
}
type beginRequest struct {
	Role     uint16
	Flags    uint8
	Reserved [5]uint8
}

func (self *header) String() string {
	return fmt.Sprintf("<ver: %d kind: %d id: %d len: %d pad: %d>", self.Version, self.Kind, self.ReqId, self.ContentLength, self.PaddingLength)
}

func newHeader(kind uint8, id uint16, content_length int) *header {
	return &header{
		Version:       1,
		Kind:          kind,
		ReqId:         id,
		ContentLength: uint16(content_length),
		PaddingLength: uint8(-content_length & 7),
	}
}

// this will read ContentLength+PaddingLength bytes, and return the first ContentLength bytes
func (self *header) readContent(r io.Reader) (b []byte, err os.Error) {
	t := int(self.ContentLength) + int(self.PaddingLength)
	b = make([]byte, t)
	if t == 0 {
		return b, nil
	}
	n := 0
	// dont allow short reads, we either get it all or we fail because of an error
	// if anything gets left on the wire, or goes unread, the whole connection is hosed
	for n < t {
		m, err := r.Read(b[n:])
		n += m
		if err != nil {
			return b[0:n], os.NewError(fmt.Sprint("Short read only got ", n, "of", t, "bytes:", err))
		}
	}
	// discard the padding bytes from the final selection
	return b[0:self.ContentLength], nil
}

// so we dont have to allocate new ones all the time, these are always zero,
// and we write slices of it for padding
var paddingSource = make([]byte, 256)

func (self *header) writePadding(w io.Writer) os.Error {
	p := self.PaddingLength
	if p > 0 {
		if p > 7 {
			return os.NewError(fmt.Sprint("writeRecord: invalid padding requested:", p, "should be less than 8."))
		}
		pad := paddingSource[0:p]
		// dont allow for short writes unless there is an error to explain it
		n := 0
		for uint8(n) < p {
			m, err := w.Write(pad[n:])
			if err != nil {
				return err
			}
			n += m
		}
	}
	return nil
}


// writeRecord writes a single FastCGI record to a Writer, being careful to write the correct padding.
// now uses a lockable RWC so that the header-body-padding writes are atomic on this conn
func writeRecord(conn *lockReadWriteCloser, kind uint8, reqId uint16, b []byte) (err os.Error) {
	conn.w.Lock()
	defer conn.w.Unlock()
	h := newHeader(kind, reqId, len(b))
	if err = binary.Write(conn, binary.BigEndian, h); err != nil {
		Log("writeRecord[", reqId, "]: error", err)
		return err
	}
	if len(b) > 0 {
		// make sure we dont have any short Writes without an error
		n := 0
		for n < len(b) {
			m, err := conn.Write(b[n:])
			if err != nil {
				return err
			}
			n += m
		}
		if err = h.writePadding(conn); err != nil {
			return err
		}
	}
	return err
}

func writeBeginRequest(conn *lockReadWriteCloser, reqid uint16, role uint16, flags uint8) (n int, err os.Error) {
	conn.w.Lock()
	defer conn.w.Unlock()
	if err = binary.Write(conn, binary.BigEndian, newHeader(typeBeginRequest, reqid, 8)); err != nil {
		return 0, err
	}
	if err = binary.Write(conn, binary.BigEndian, beginRequest{
		Role:  roleResponder,
		Flags: flags,
	}); err != nil {
		return 0, err
	}
	// Log("writeRecord[",reqid,"]: beginRequest")
	return 16, nil
}

func writeEndRequest(conn *lockReadWriteCloser, reqid uint16, appStatus uint32, protocolStatus uint8) (n int, err os.Error) {
	conn.w.Lock()
	defer conn.w.Unlock()
	if err = binary.Write(conn, binary.BigEndian, newHeader(typeEndRequest, reqid, 8)); err != nil {
		return 0, err
	}
	if err = binary.Write(conn, binary.BigEndian, endRequest{
		AppStatus:      appStatus,
		ProtocolStatus: protocolStatus,
	}); err != nil {
		return 8, err
	}
	// Log("writeRecord[",reqid,"]: endRequest")
	return 16, nil
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

func encodeSize(size int) []byte {
	if size > 127 {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, uint32(size))
		return buf
	}
	buf := make([]byte, 1)
	buf[0] = uint8(size)
	return buf
}

// when the webserver sends us "ACCEPT_ENCODING" as a header,
// (in the typeParams records) standardize it like: Accept-Encoding
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
			ret[i] = bytes.ToLower(str[i : i+1])[0]
		}
	}
	return string(ret)
}

// a reader that supports a dummy Close()
type nopCloser struct {
	io.Reader
}

func (nopCloser) Close() os.Error { return nil }

// copied from http.request
// this does the atoi conversion at different offsets i in s
// used in parsing the HTTP protocol version
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

// we need to synchronize multiplexed access to the same I/O connections
// unlike the RWMutex in the sync package, the Read lock cannot be open
// by an arbitrary number of readers, only one at a time
type lockReadWriteCloser struct {
	io.ReadWriteCloser
	w sync.Mutex
	r sync.Mutex
}
