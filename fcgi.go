/* fcgigo is a FastCGI implementation in pure Go.

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
	// "io/ioutil"
	// "container/vector"
)

/* Handler is the type for a callable that handles requests. */
type Handler func(*Request)

/* The basic FastCGI Request object */
type Request struct {
	// embed all the fields and methods of http.Request
	*http.Request

	// also, a response
	*http.Response

	// Data reads the FCGI_DATA channel from the webserver. (for file uploads I think?)
	Data io.Reader
	data *bytes.Buffer // the private buffer behind Data

	// the requestId from the FCGI protocol
	id uint16

	// as FCGI_STDIN packets arrive, they go here
	stdin *bytes.Buffer // read by .Body

	// as FCGI_PARAM packets arrive, they go here in raw form
	env map[string]string // HTTP_* values are parsed out, canonicalized, and stored in .Header

	// for the response
	// headers    map[string]string
	// status     string
	// statusCode int

	// book-keeping
	responseStarted bool
	startTime       int64
	real_output     io.WriteCloser
}

/* Run accepts new connections on stdin, and serves FCGI over the accepted connections.

This is how, for example, lighttpd expects you to behave if you specify "bin-path" in its config.
Arguments:
	application - the callable which will produce the output for each Request.
	pool_size - the number of goroutines to spawn into a worker pool for processing requests
*/
func Run(application Handler, pool_size int) os.Error {
	pool := newWorkerPool(pool_size, application)
	for {
		nfd, _, err := syscall.Accept(0)
		// Log("Accept: nfd: %d addr:%s err:%s\r\n",nfd,addr,err);
		if err != 0 {
			Log("Accept Error: %d", err)
			break
		}
		go handleConnection(io.ReadWriteCloser(os.NewFile(nfd, "<noname>")), pool)
	}
	pool.stopAllWorkers()
	return nil
}

/* RunTCP creates a FastCGI Responder that listens on the supplied address.
Arguments:
 	addr - a string like "localhost:1234", or "0.0.0.0:999", that specifies a local interface to listen on.
 	application - the callable which will produce the output for each Request.
 	pool_size - the number of goroutines to spawn into a worker pool for processing requests
*/
func RunTCP(addr string, application Handler, pool_size int) os.Error {
	a, e := net.ResolveTCPAddr(addr)
	if e != nil {
		return e
	}
	s, e := net.ListenTCP("tcp4", a)
	if e != nil {
		return e
	}
	Log("Listening")
	pool := newWorkerPool(pool_size, application)
	for {
		rw, e := s.AcceptTCP()
		if e != nil {
			Log("Accept error: %s", e)
			break
		}
		go handleConnection(io.ReadWriteCloser(rw), pool)
	}
	pool.stopAllWorkers()
	s.Close()
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
	req.fcgi_write(FCGI_STDERR, str)
	// TODO: req.fcgi_write(FCGI_STDOUT, <error output html>)
}

/* Write(msg) sends text over the FCGI_STDOUT channel to the webserver. */
func (req *Request) Write(str string) {
	if !req.responseStarted {
		req.startResponse(req.Response.Status, req.Response.Header)
	}
	req.fcgi_write(FCGI_STDOUT, str)
}

/* Private Request methods */
func newRequest(id uint16, output io.WriteCloser) *Request {
	start := time.Nanoseconds()
	stdin := bytes.NewBuffer(make([]byte, 0, 4096))
	data := bytes.NewBuffer(make([]byte, 0, 4096))
	r := &Request{
		Request: &http.Request{
			Method: "GET",
			RawURL: "",
			URL: nil,
			Proto: "",
			ProtoMajor: 0,
			ProtoMinor: 0,
			Header: map[string]string{},
			Body: io.Reader(stdin),
			Close: false,
			Host: "",
			Referer: "",
			UserAgent: "",
			Form: nil,
		},
		Response: &http.Response{
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
		// book-keeping
		responseStarted: false,
		startTime: start,
		real_output: output,
	}
	r.Close = true
	r.Data = io.Reader(data)
	Log("Returning r")
	return r
}
func (req *Request) fcgi_write(kind uint8, str string) {
	// the private cousin of req.Write(), this one outputs the real fcgi packets to the webserver
	req.real_output.Write(newFCGIPacketString(kind, req.id, str).bytes())
}
func (req *Request) end(appStatus uint32, protocolStatus uint8) {
	// send the done messages
	req.abort()
	// write the final packet
	req.real_output.Write(newFCGIPacket(FCGI_END_REQUEST, req.id, newEndRequest(appStatus, protocolStatus).bytes()).bytes())
	// if the webserver requested that we close this connection
	if req.Close {
		// then close it
		req.real_output.Close()
	}
}
func (req *Request) startResponse(status string, headers map[string]string) {
	req.responseStarted = true
	req.Write("Status: " + status + "\r\n")
	for key, val := range headers {
		req.Write(key + ": " + val + "\r\n")
	}
	req.Write("\r\n")
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
	// make sure our Request object is complete before we dispatch
	req.Host = req.env["HTTP_HOST"]
	req.Method = req.env["REQUEST_METHOD"]
	req.RawURL = req.env["REQUEST_URI"]
	req.URL, _ = http.ParseURLReference("http://" + req.env["HTTP_HOST"] + req.RawURL + "?" + req.env["QUERY_STRING"])
	req.Proto = req.env["SERVER_PROTOCOL"] // like HTTP/1.1
	i := 5
	req.ProtoMajor, i, _ = atoi(req.Proto, i)
	req.ProtoMinor, _, _ = atoi(req.Proto, i+1)
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
func (req *Request) abort() {
	req.fcgi_write(FCGI_STDOUT, "")
	req.fcgi_write(FCGI_STDERR, "")
}

/* FastCGI has its own pair encoding: <name-len><val-len><name><val>, with a couple kinks.
 * but these two functions read a chunk at a time, see processParams for usage
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
func (req *Request) processParams(text []byte) {
	// processParams reads an encoded []byte into this
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
		// Log("Param: %s val: %s", key, val)
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
	requests := map[uint16]*Request{}
	for {
		p, err := readFCGIPacket(io.Reader(rw))
		if err == os.EOF {
			break
		} else if err != nil {
			os.Stderr.WriteString(err.String() + "\r\n")
			Log("Error: %s", err.String())
			break
		}
		if p == nil {
			break
		}
		req, _ := requests[p.hdr.RequestId]
		switch p.hdr.Kind {
		case FCGI_BEGIN_REQUEST:
			// Log("FCGI_BEGIN_REQUEST")
			// TODO: since we dont read the real Close flag down below, we can skip reading the content
			// var h FCGIBeginRequest;
			// binary.Read(bytes.NewBuffer(p.content), binary.BigEndian, &h);
			req = newRequest(p.hdr.RequestId, io.WriteCloser(rw))
			// lighttpd sets this backwards atm, so we cant use the real value:
			// req.Close = (h.Flags == 0);
			// TODO: find/write a webserver that supports multiplexed fastcgi
			req.Close = true
			// fmt.Printf("setting req.Close = %s from %d", req.Close, h.Flags);
			requests[p.hdr.RequestId] = req
		case FCGI_PARAMS:
			// Log("FCGI_PARAMS")
			req.processParams(p.content)
		case FCGI_STDIN:
			// Log("FCGI_STDIN")
			if p.hdr.ContentLength == uint16(0) { // once the request body is in, start the application
				// TODO: can we do this a step earlier, so that the stdin Reader blocks until these packets arrive?
				pool.assignWork(req) // send the request into the worker pool
			} else {
				Log("STDIN: %s", p.content)
				req.stdin.Write(p.content)
			}
		case FCGI_DATA:
			Log("FCGI_DATA")
			if p.hdr.ContentLength > uint16(0) {
				req.data.Write(p.content)
			}
		case FCGI_ABORT_REQUEST:
			Log("ABORT_REQUEST recieved")
			req.abort()
		default:
			Log("Unknown packet header type: %d", p.hdr.Kind)
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

var ( // from fcgi.h
	FCGI_REQUEST_COMPLETE  = uint8(0)
	FCGI_BEGIN_REQUEST     = uint8(1)
	FCGI_ABORT_REQUEST     = uint8(2)
	FCGI_END_REQUEST       = uint8(3)
	FCGI_PARAMS            = uint8(4)
	FCGI_STDIN             = uint8(5)
	FCGI_STDOUT            = uint8(6)
	FCGI_STDERR            = uint8(7)
	FCGI_DATA              = uint8(8)
	FCGI_GET_VALUES        = uint8(9)
	FCGI_GET_VALUES_RESULT = uint8(10)
	FCGI_UNKNOWN_TYPE      = uint8(11)
	FCGI_MAXTYPE           = (FCGI_UNKNOWN_TYPE)
)

type FCGIHeader struct {
	Version       uint8
	Kind          uint8
	RequestId     uint16
	ContentLength uint16
	PaddingLength uint8
	Reserved      uint8
}

func newFCGIHeader(r io.Reader) (FCGIHeader, os.Error) {
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
	order.PutUint16(buf[2:4], h.RequestId)
	order.PutUint16(buf[4:6], h.ContentLength)
	buf[6] = h.PaddingLength
	buf[7] = h.Reserved
	return buf
}

type FCGIPacket struct {
	hdr     FCGIHeader
	content []byte
}

func readFCGIPacket(r io.Reader) (*FCGIPacket, os.Error) {
	p := new(FCGIPacket)
	hdr, err := newFCGIHeader(r)
	if err != nil {
		return nil, err
	}
	p.hdr = hdr
	if p.hdr.ContentLength > 0 {
		// NOTE: we read the extra padding bytes here, and discard them below
		p.content = make([]byte, p.hdr.ContentLength+uint16(p.hdr.PaddingLength))
		_, err = r.Read(p.content)
		if err != nil {
			return nil, err
		}
		p.content = p.content[0:p.hdr.ContentLength] // leave the padding bytes sitting in memory?
	}
	return p, nil
}
func newFCGIPacket(kind uint8, reqid uint16, content []byte) *FCGIPacket {
	l := len(content)
	p := &FCGIPacket{
		hdr: FCGIHeader{
			Version: 1,
			Kind: kind,
			RequestId: reqid,
			ContentLength: uint16(l),
			PaddingLength: uint8(-l & 7),
		},
		content: content,
	}
	return p
}
func newFCGIPacketString(kind uint8, reqid uint16, content string) *FCGIPacket {
	buf := bytes.NewBufferString(content)
	return newFCGIPacket(kind, reqid, buf.Bytes())
}
func (p *FCGIPacket) bytes() []byte {
	buf := bytes.NewBuffer(p.hdr.bytes())
	buf.Write(p.content)
	for i := uint8(0); i < p.hdr.PaddingLength; i++ {
		buf.WriteByte(0)
	}
	return buf.Bytes()
}

type FCGIBeginRequest struct {
	Role     uint16
	Flags    uint8
	Reserved [5]uint8
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
	// os.Stderr.WriteString("Log: "+msg+"\r\n");
}
