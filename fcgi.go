/* fcgi.go is a FastCGI implementation in pure Go.

Minimal example:
fcgi.Responder(func (req *fcgi.Request) {
	req.Status("200 OK")
	req.Write("Hello World")
}, 100)

The above example would result in a program that can be launched by the webserver.

Or, to spawn an external process that listens on a TCP port:

fcgi.TCPResponder("localhost:7143", func (req *fcgi.Request) {
	req.Status("200 OK")
	req.Write("Hello World")
}, 100)

*/
package fcgi

import (
	"os"
	"io"
	"net"
	"fmt"
	"encoding/binary"
	"bytes"
	"strings"
	"http"
	"time"
	"syscall"
	// "log"
	// "io/ioutil"
	// "container/vector"
)

type RequestId uint16
type Socket io.ReadWriteCloser

/* Handler is the type for a callable that handles requests. */
type Handler func(*Request)

/* The basic FastCGI Request object */
type Request struct {
	// embed all the fields and methods of http.Request
	http.Request

	// also, a response
	http.Response

	// Data reads the FCGI_DATA channel from the webserver. (for file uploads I think?)
	Data io.Reader
	data *bytes.Buffer // the private buffer behind Data

	// the requestId from the FCGI protocol
	id RequestId

	// as FCGI_STDIN packets arrive, they go here
	stdin *bytes.Buffer // read by .Body Reader

	// as FCGI_PARAM packets arrive, they go here in raw form
	env map[string]string // HTTP_* values are parsed out of this map, cleaned, and stored in .Header

	// book-keeping
	responseStarted bool
	startTime       int64
	real_output     io.WriteCloser
	chunked         bool // if the response didnt provide a Content-Length or Transfer-Encoding of its own
	// then Transfer-Encoding: chunked will be assumed, and all calls to .Write will add the chunk headers automatically
}

/* Responder accepts new connections on stdin, and serves FCGI over the accepted connections.
This is how, for example, lighttpd expects you to behave if you specify "bin-path" in its config.
Arguments:
	application - the callable which will produce the output for each Request.
	pool_size - the number of goroutines to spawn into a worker pool for processing requests
*/
type Responder struct {
	pool *WorkerPool
}

func NewResponder(application Handler, pool_size int) *Responder {
	r := &Responder{
		pool: newWorkerPool(pool_size, application),
	}
	return r
}
func (self *Responder) Run(ready chan bool, exit chan bool, done chan bool) {
	accept := AcceptFDChannel(0)
	for {
		select {
		case nfd := <-accept:
			if nfd > 0 {
				Log("Responder: Accept(0)")
				go handleConnection(io.ReadWriteCloser(os.NewFile(nfd, "<noname>")), self.pool)
			} else {
				break
			}
		case <-exit:
			break
		}
	}
	Log("Responder: Done.")
	done <- true
}

/* TCPResponder creates a FastCGI Responder that listens on the supplied address.
Arguments:
 	addr - a string like "localhost:1234", or "0.0.0.0:999", that specifies a local interface to listen on.
 	application - the callable which will produce the output for each Request.
 	pool_size - the number of goroutines to spawn into a worker pool for processing requests
*/
type TCPResponder struct {
	listenAddress string
	listenPort    int
	pool          *WorkerPool
}

func NewTCPResponder(listenAddress string, application Handler, pool_size int) *TCPResponder {
	self := &TCPResponder{
		listenAddress: listenAddress,
		pool: newWorkerPool(pool_size, application),
	}
	return self
}
func (self *TCPResponder) Run(ready chan bool, exit chan bool, done chan bool) os.Error {
	defer Done(done)
	if addr, err := net.ResolveTCPAddr(self.listenAddress); err == nil {
		self.listenPort = addr.Port
		if lsock, err := net.ListenTCP("tcp4", addr); err == nil {
			accept := AcceptChannel(lsock)
			ready <- true
			for {
				select {
				case sock := <-accept:
					if sock != nil {
						go handleConnection(sock, self.pool)
					} else {
						break
					}
				case <-exit:
					break
				}
			}
		} else {
			Log("ListenTCP failed: %s", err.String())
			return err
		}
	} else {
		Log("ResolveTCPAddr failed: %s", err.String())
		return err
	}
	return nil
}

/* Sets the response status. */
func (req *Request) SetStatus(status string) {
	if !req.responseStarted {
		req.Response.Status = status
		req.Response.StatusCode, _, _ = atoi(status, 0)
	}
}

/* Sets a response header. */
func (req *Request) SetHeader(str string, val string) {
	if !req.responseStarted {
		req.Response.Header[str] = val
	}
}

/* Gets a request header. */
func (req *Request) GetHeader(name string) (string, bool) {
	ret, ok := req.Request.Header[name]
	return ret, ok
}

/* Error(msg) sends text over the FCGI_STDERR channel to the webserver. */
func (req *Request) Error(str string) {
	if !req.responseStarted {
		req.startResponse("500 Application Error", req.Response.Header)
	}
	req.fcgiWrite(FCGI_STDERR, str)
	// TODO: req.fcgiWrite(FCGI_STDOUT, <error output html>)
}

/* Write(msg) sends text over the FCGI_STDOUT channel to the webserver. */
func (req *Request) Write(str string) {
	if !req.responseStarted {
		req.startResponse(req.Response.Status, req.Response.Header)
	}
	if req.chunked {
		req.fcgiWrite(FCGI_STDOUT, addChunkHeaders(str))
	} else {
		req.fcgiWrite(FCGI_STDOUT, str)
	}
}

func addChunkHeaders(str string) string {
	l := len(str)
	hex := fmt.Sprintf("%x", l)
	crlf := "\r\n"
	return hex + crlf + str + crlf
}


/* Private Request methods */
func newRequest(id RequestId, output io.WriteCloser) *Request {
	start := time.Nanoseconds()
	stdin := bytes.NewBuffer(make([]byte, 0, 4096))
	data := bytes.NewBuffer(make([]byte, 0, 4096))
	r := &Request{
		Request: http.Request{
			Method: "GET",
			RawURL: "",
			URL: nil,
			Proto: "",
			ProtoMajor: 0,
			ProtoMinor: 0,
			Header: map[string]string{},
			Body: NewDummyReadCloser(stdin),
			Close: true,
			Host: "",
			Referer: "",
			UserAgent: "",
			Form: nil,
		},
		Response: http.Response{
			Status: "200 OK",
			StatusCode: 200,
			Header: map[string]string{},
		},
		// the fastcgi requestId
		id: id,
		// the raw environment sent over FCGI
		env: map[string]string{},
		// the private buffers to write to
		stdin: stdin,
		data: data,
		// the public read-only Data buffer for the FCGI_DATA stream
		Data: io.Reader(data),
		// book-keeping
		responseStarted: false,
		startTime: start,
		real_output: output,
		chunked: false,
	}
	return r
}
func (req *Request) fcgiWrite(kind uint8, str string) {
	// the private cousin of req.Write(), this one outputs the real fcgi packets to the webserver
	WriteFCGI(req.real_output, newFCGIPacketString(kind, req.id, str))
}
func (req *Request) end(appStatus uint32, protocolStatus uint8) {
	if req.chunked {
		// send the last-chunk
		req.chunked = false // we want raw output for this last part
		req.Write("0\r\n")
	}
	// send the done messages
	req.finish()
	// write the final packet
	req.real_output.Write(newFCGIPacket(FCGI_END_REQUEST, req.id, newEndRequest(appStatus, protocolStatus).bytes()).bytes())
	// if the webserver requested that we close this connection
	if req.Request.Close {
		req.real_output.Close()
	}
}
func (req *Request) startResponse(status string, headers map[string]string) {
	req.responseStarted = true
	crlf := "\r\n"
	req.Write(req.Request.Proto + " " + status + crlf)
	chunked := false
	// if there is no Content-Length or Transfer-Encoding
	// automatically add Transfer-Encoding: chunked
	if _, ok := headers["Content-Length"]; !ok {
		if _, ok := headers["Transfer-Encoding"]; !ok {
			headers["Transfer-Encoding"] = "chunked"
			chunked = true
		} // if someone manually set Transfer-Encoding before now
		// we take it as an indication that they want to manually do the encoding
		// and all .Write()s will be untouched
	}

	// force a default Content-Type
	if _, ok := headers["Content-Type"]; !ok {
		headers["Content-Type"] = "plain/text"
	}
	for key, val := range headers {
		req.Write(key + ": " + val + crlf)
	}
	req.Write(crlf)
	req.chunked = chunked
}
// copied from http.request
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
func (req *Request) handle(application Handler) {
	// make sure our http.Request object is complete before we dispatch
	var ok bool
	if req.Host, ok = req.env["HTTP_HOST"]; !ok {
		req.Error("fcgi.go: HTTP_HOST required and missing from FCGI_PARAMS.")
		req.Write("fcgi.go: HTTP_HOST required and missing from FCGI_PARAMS.")
		req.end(500, FCGI_REQUEST_COMPLETE)
		return
	}
	if req.Method, ok = req.env["REQUEST_METHOD"]; !ok {
		req.Error("fcgi.go: REQUEST_METHOD required and missing from FCGI_PARAMS.")
		req.Write("fcgi.go: REQUEST_METHOD required and missing from FCGI_PARAMS.")
		req.end(500, FCGI_REQUEST_COMPLETE)
		return
	}
	if req.RawURL, ok = req.env["REQUEST_URI"]; !ok {
		req.Error("fcgi.go: REQUEST_URI required and missing from FCGI_PARAMS.")
		req.Write("fcgi.go: REQUEST_URI required and missing from FCGI_PARAMS.")
		req.end(500, FCGI_REQUEST_COMPLETE)
		return
	}
	urlstr := "http://" + req.Host + req.RawURL + "?" + req.env["QUERY_STRING"]
	if url, err := http.ParseURLReference(urlstr); err == nil {
		req.URL = url
	} else {
		req.Error("fcgi.go: Could not parse a proper URL from: " + urlstr)
		req.Write("fcgi.go: Could not parse a proper URL from: " + urlstr)
		req.end(500, FCGI_REQUEST_COMPLETE)
	}
	req.Request.Proto = req.env["SERVER_PROTOCOL"] // like HTTP/1.1
	if req.Request.Proto[0:4] != "HTTP" {
		req.Error(fmt.Sprint("Request protocol not HTTP: %s", string(req.Request.Proto[0:4])))
		req.Write(fmt.Sprint("Request protocol not HTTP: %s", string(req.Request.Proto[0:4])))
		req.end(500, FCGI_REQUEST_COMPLETE)
	}
	i := 5
	req.Request.ProtoMajor, i, _ = atoi(req.Request.Proto, i)
	req.Request.ProtoMinor, _, _ = atoi(req.Request.Proto, i+1)
	req.Response.Proto = req.Request.Proto
	req.Response.ProtoMajor = req.Request.ProtoMajor
	req.Response.ProtoMinor = req.Request.ProtoMinor

	if ref, ok := req.env["HTTP_REFERER"]; ok {
		req.Referer = ref
	}
	if agent, ok := req.env["HTTP_USER_AGENT"]; ok {
		req.UserAgent = agent
	}

	// make sure req.Form[] is built
	req.ParseForm()

	// call the application
	application(req)

	// end the FCGI request
	req.end(200, FCGI_REQUEST_COMPLETE)
}
func (req *Request) finish() {
	req.fcgiWrite(FCGI_STDOUT, "")
	req.fcgiWrite(FCGI_STDERR, "")
}

/* FastCGI has its own pair encoding: <name-len><val-len><name><val>, with a couple kinks.
 * but these two functions read a chunk at a time, see parseFcgiParams for usage
 */
func getOneSize(slice []byte) (uint32, []byte) {
	size := slice[0]
	r := 1
	if size>>7 == 1 {
		er := binary.Read(bytes.NewBuffer(slice[0:4]), binary.BigEndian, &size)
		if er != nil {
			os.Stderr.WriteString(fmt.Sprintf("binary.Read error: %s\r\n", er))
			return 0, slice[len(slice):len(slice)]
		}
		r = 4
	}
	return uint32(size), slice[r:len(slice)]
}
func getOneValue(slice []byte, size uint32) (string, []byte) {
	if int(size) > len(slice) {
		Log("Responder: invalid pair encoding", slice, size)
		return "", slice[0:0]
	}
	return string(slice[0:size]), slice[size:len(slice)]
}
func standardCase(str []byte) string {
	// when the webserver sends us "ACCEPT_ENCODING" as a header,
	// standardize it like: Accept-Encoding
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
func (req *Request) parseFcgiParams(text []byte) {
	// parseFcgiParams reads an encoded []byte into this
	// request's env map
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
		req.env[key] = val
		Log("Param: %s val: %s", key, val)
		if strings.HasPrefix(key, "HTTP_") {
			req.Request.Header[standardCase(strings.Bytes(key)[5:])] = val
		}
	}
}

/* handleConnection is the handler that reads actual FCGI packets off the wire,
 * it fills in data on the proper request, and once the body of the request is in,
 * it dispatches the request to the specified worker pool
 */
func handleConnection(rw io.ReadWriteCloser, pool *WorkerPool) {
	requests := map[RequestId]*Request{}
	for {
		Log("Responder: Reading Packet...")
		p, err := ReadFCGI(rw)
		if err == os.EOF {
			Log("Responder: EOF")
			break
		} else if err != nil {
			Log("Responder: ReadFCGI Error:", err)
			break
		}
		if p == nil {
			Log("Responder: got a nil packet from ReadFCGI")
			break
		}
		Log("Responder: got packet %s", p)
		req, _ := requests[p.hdr.ReqId]
		switch p.hdr.Kind {
		case FCGI_BEGIN_REQUEST:
			Log("Responder: FCGI_BEGIN_REQUEST")
			var h FCGIBeginRequest
			binary.Read(bytes.NewBuffer(p.content), binary.BigEndian, &h)
			req = newRequest(p.hdr.ReqId, io.WriteCloser(rw))
			req.Request.Close = ((h.Flags & FCGI_KEEP_CONN) != FCGI_KEEP_CONN)
			req.Response.Close = req.Request.Close
			fmt.Printf("Responder: setting req.Close = %s from %d", req.Request.Close, h.Flags)
			requests[p.hdr.ReqId] = req
		case FCGI_PARAMS:
			Log("Responder: FCGI_PARAMS")
			req.parseFcgiParams(p.content)
			Log("Responder: done parsing.")
		case FCGI_STDIN:
			Log("Responder: FCGI_STDIN")
			if p.hdr.ContentLength == uint16(0) { // once the request body is in, start the application
				// TODO: can we do this a step earlier, so that the stdin Reader blocks until these packets arrive?
				pool.assignWork(req) // send the request into the worker pool
			} else {
				// Log("STDIN: %s", p.content)
				req.stdin.Write(p.content)
			}
		case FCGI_GET_VALUES:
			Log("Responder: FCGI_GET_VALUES")
			// TODO
		case FCGI_DATA:
			Log("Responder: FCGI_DATA")
			if p.hdr.ContentLength > uint16(0) {
				req.data.Write(p.content)
			}
		case FCGI_ABORT_REQUEST:
			Log("Responder: ABORT_REQUEST")
			req.finish()
		default:
			Log("Responder: Unknown packet header type: %d in %s", p.hdr.Kind, p)
			// req.real_output.Write(newFCGIPacket(FCGI_UNKNOWN_TYPE, p.hdr.ReqId, []byte{p.hdr.Kind, 0, 0, 0, 0, 0, 0, 0}).bytes())
		}
	}
	rw.Close()
}


/* Worker Pool Definitions */
type WorkerPool struct {
	ch   chan *Request
	done chan int
	n    int
}

func newWorkerPool(pool_size int, h Handler) *WorkerPool {
	p := &WorkerPool{
		ch: make(chan *Request, pool_size),
		done: make(chan int),
		n: pool_size,
	}
	Log("Starting worker pool (%d)", pool_size)
	for i := 0; i < pool_size; i++ {
		// spawn a worker goroutine into the pool
		go p.worker(i, h)
	}
	return p
}
func (self *WorkerPool) assignWork(req *Request) {
	self.ch <- req
}
func (self *WorkerPool) stopAllWorkers() {
	for i := 0; i < self.n; i++ {
		self.ch <- nil // send a close signal
		<-self.done    // wait for ack
	}
}
func (self *WorkerPool) worker(id int, application Handler) {
	// each worker continually
	for {
		// reads from their work channel
		req := <-self.ch
		// breaks on a close signal (a nil value)
		if req == nil {
			break
		}
		// handles the request
		req.handle(application)
		elapsed := float64(time.Nanoseconds()-req.startTime) * float64(10e-6)
		Log("Worker %d completed: %s in %.2f ms.", id, req.env["REQUEST_URI"], elapsed)
	}
	// Log("Worker %d exiting", id)
	// when finished, notify
	self.done <- 1
}

/* The protocol details: */

const ( // packet types
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

const FCGI_KEEP_CONN = 1
const FCGI_MAX_WRITE = 65534

const (
	FCGI_RESPONDER = iota + 1
	FCGI_AUTHORIZER
	FCGI_FILTER
)

const ( // protocolStatus
	FCGI_REQUEST_COMPLETE = iota
	FCGI_CANT_MPX_CONN
	FCGI_OVERLOADED
	FCGI_UNKNOWN_ROLE
)

type FCGIHeader struct {
	Version       uint8
	Kind          uint8
	ReqId         RequestId
	ContentLength uint16
	PaddingLength uint8
	Reserved      uint8
}

func readFCGIHeader(r io.Reader) (FCGIHeader, os.Error) {
	var h FCGIHeader
	err := binary.Read(r, binary.BigEndian, &h)
	if err != os.EOF && err != nil {
		Log("newFCGIHeader Error: %s %s %s", err, r, h)
	}
	return h, err
}
func (h *FCGIHeader) bytes() []byte {
	order := binary.BigEndian
	buf := make([]byte, 8)
	buf[0] = h.Version
	buf[1] = h.Kind
	order.PutUint16(buf[2:4], uint16(h.ReqId))
	order.PutUint16(buf[4:6], h.ContentLength)
	buf[6] = h.PaddingLength
	buf[7] = h.Reserved
	return buf
}

type FCGIPacket struct {
	hdr     FCGIHeader
	content []byte
}

func ReadFCGI(r io.Reader) (*FCGIPacket, os.Error) {
	p := new(FCGIPacket)
	if hdr, err := readFCGIHeader(r); err == nil {
		p.hdr = hdr
		if p.hdr.ContentLength > 0 || p.hdr.PaddingLength > 0 {
			// read the content (and any padding)
			p.content = make([]byte, p.hdr.ContentLength+uint16(p.hdr.PaddingLength))
			if _, err = r.Read(p.content); err != nil {
				return nil, err
			}
			// discard the padding
			p.content = p.content[0:p.hdr.ContentLength]
		}
		return p, nil
	} else {
		return nil, err
	}
	return nil, nil
}

func WriteFCGI(w io.Writer, p *FCGIPacket) { w.Write(p.bytes()) }

func newFCGIPacket(kind uint8, reqid RequestId, content []byte) *FCGIPacket {
	l := len(content)
	p := &FCGIPacket{
		hdr: FCGIHeader{
			Version: 1,
			Kind: kind,
			ReqId: reqid,
			ContentLength: uint16(l),
			PaddingLength: uint8(-l & 7),
		},
		content: content,
	}
	return p
}
func newFCGIPacketString(kind uint8, reqid RequestId, content string) *FCGIPacket {
	return newFCGIPacket(kind, reqid, strings.Bytes(content))
}
func (p *FCGIPacket) bytes() []byte {
	buf := bytes.NewBuffer(p.hdr.bytes())
	buf.Write(p.content)
	buf.Write(make([]byte, p.hdr.PaddingLength))
	return buf.Bytes()
}

type FCGIBeginRequest struct {
	Role     uint16
	Flags    uint8
	Reserved [5]uint8
}

func newFCGIBeginRequest(keepConnected bool) *FCGIBeginRequest {
	flags := uint8(0)
	if keepConnected {
		flags |= FCGI_KEEP_CONN
	}
	return &FCGIBeginRequest{
		Role: FCGI_RESPONDER,
		Flags: flags,
	}
}
func (self *FCGIBeginRequest) bytes() []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint16(buf, self.Role)
	buf[2] = self.Flags
	return buf
}

type FCGIEndRequest struct {
	appStatus      uint32
	protocolStatus uint8
	reserved       [3]uint8
}

func newEndRequest(appStatus uint32, protocolStatus uint8) *FCGIEndRequest {
	er := new(FCGIEndRequest)
	er.appStatus = appStatus
	er.protocolStatus = protocolStatus
	return er
}
func (self *FCGIEndRequest) bytes() []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf, self.appStatus)
	buf[4] = self.protocolStatus
	return buf
}

/* some util stuff */
func Log(msg string, v ...) {
	if strings.Index(msg, "%") > -1 {
		msg = fmt.Sprintf(msg+"\r\n", v)
	} else {
		msg = msg + " " + fmt.Sprintln(v)
	}
	f, err := os.Open("fcgi.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		os.Stderr.WriteString("LogErr1: " + err.String() + "\r\n")
	}
	n, err := f.WriteString(msg)
	if err != nil {
		os.Stderr.WriteString("LogErr2: " + err.String() + "\r\n")
	}
	if n == 0 {
		os.Stderr.WriteString("LogErr3: 0 bytes written\r\n")
	}
	err = f.Close()
	if err != nil {
		os.Stderr.WriteString("LogErr: " + err.String() + "\r\n")
	}
	// log.Stderr(msg)
	// os.Stderr.WriteString("Log: "+msg+"\r\n")
}

type ProxyReader struct {
	io.Reader
	r io.Reader
}

func NewProxyReader(r io.Reader) *ProxyReader { return &ProxyReader{r: r} }
func (self *ProxyReader) Read(b []byte) (n int, err os.Error) {
	n, err = self.r.Read(b)
	Log("Read: n:%d b:%s err: %s\r\n", n, b, err)
	return n, err
}

type DummyReadCloser struct {
	io.Reader
}

func NewDummyReadCloser(r io.Reader) DummyReadCloser {
	return DummyReadCloser{
		Reader: r,
	}
}
func (r DummyReadCloser) Close() os.Error { return nil }

// returns a channel that, when read, yields the results of AcceptTCP()
func AcceptChannel(listenSock *net.TCPListener) (ch chan *net.TCPConn) {
	ch = make(chan *net.TCPConn)
	go func(c chan *net.TCPConn) {
		for {
			if sock, err := listenSock.AcceptTCP(); err == nil {
				c <- sock
			} else {
				c <- nil
				break
			}
		}
	}(ch)
	return ch
}

// returns a channel that, when read, yields the results of syscall.Accept(fd)
func AcceptFDChannel(fd int) chan int {
	ch := make(chan int)
	go func() {
		for {
			if sock, _, err := syscall.Accept(fd); err == 0 {
				ch <- sock
			} else {
				ch <- -1
				break
			}
		}
	}()
	return ch
}

func Done(done chan bool) { done <- true }
