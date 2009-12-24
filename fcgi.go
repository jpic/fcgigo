/** fcgigo is a FastCGI implementation in pure Go.
 * Minimal example:
 * fcgi.Run(func (req *fcgi.Request) {
 *	req.Status("200 OK");
 *	req.Write("Hello World");
 * }, 100);
 *
 * The above example would result in a program that can be launched by the webserver.
 *
 * Or, to spawn an external process that listens on a TCP port:
 * fcgi.RunTCP("localhost:7143", func (req *fcgi.Request) {
 *	req.Status("200 OK");
 *	req.Write("Hello World");
 * }, 100);
 **/
package fcgi

import (
	"os"
	"io"
	"net"
	"fmt"
	"encoding/binary"
	"bytes"
	"time"
	"syscall"
)

/* The type for a callable that handles requests */
type Handler func(*Request)

/** Run
 * When launched by a webserver, Run() will accept new connections on stdin.
 * (this is the standard way from the spec)
 * Arguments:
 *	application - the callable which will produce the output for each Request.
 *	pool_size - the number of goroutines to spawn into a worker pool for processing requests
 **/
func Run(application Handler, pool_size int) os.Error {
	pool := newWorkerPool(pool_size, application)
	for {
		nfd, _, err := syscall.Accept(0)
		// Log(fmt.Sprintf("Accept: nfd: %d addr:%s err:%s\r\n",nfd,addr,err));
		if err != 0 {
			Log(fmt.Sprintf("Accept Error: %d\r\n", err))
			break
		}
		go handleConnection(io.ReadWriteCloser(os.NewFile(nfd, "<noname>")), pool)
	}
	pool.stopAllWorkers()
	return nil
}

/** RunTCP
 *	Creates a FastCGI Responder that listens on the supplied address.  This functions runs forever.
 * (even though the spec doesn't mention this, it's how you would do a big cluster of remote listeners)
 * Arguments:
 *	addr - a string like "localhost:1234", or "0.0.0.0:999", that specifies a local interface to listen on.
 *	application - the callable which will produce the output for each Request.
 *	pool_size - the number of goroutines to spawn into a worker pool for processing requests
 **/
func RunTCP(addr string, application Handler, pool_size int) os.Error {
	a, e := net.ResolveTCPAddr(addr)
	if e != nil {
		return e
	}
	s, e := net.ListenTCP("tcp4", a)
	if e != nil {
		return e
	}
	Log(fmt.Sprint("Listening\r\n"))
	pool := newWorkerPool(pool_size, application)
	for {
		rw, e := s.AcceptTCP()
		if e != nil {
			Log(fmt.Sprintf("Accept error: %s\r\n", e))
			break
		}
		go handleConnection(io.ReadWriteCloser(rw), pool)
	}
	pool.stopAllWorkers()
	s.Close()
	return nil
}

/* The basic Request object for a FastCGI Request */
type Request struct {
	id uint16

	// from the request
	env   map[string]string
	stdin *bytes.Buffer
	data  *bytes.Buffer

	// for the response
	headers map[string]string
	status  string
	stdout  chan string
	stderr  chan string

	// book-keeping
	responseStarted bool
	startTime       int64
	closeOnEnd      bool
	pump_done       chan int // for signalling when a pump goroutine dies
	real_output     io.WriteCloser
}

/* Sets the response status. */
func (req *Request) Status(status string) {
	if !req.responseStarted {
		req.status = status
	}
}

/* Sets a response header. */
func (req *Request) Header(str string, val string) {
	if !req.responseStarted {
		req.headers[str] = val
	}
}

/* Sends text over the FCGI_STDERR channel to the webserver. */
func (req *Request) Error(str string) {
	if !req.responseStarted {
		req.startResponse("500 Application Error", req.headers)
	}
	req.stderr <- str
}

/* Sends text over the FCGI_STDOUT channel to the webserver. */
func (req *Request) Write(str string) {
	if !req.responseStarted {
		req.startResponse(req.status, req.headers)
	}
	req.stdout <- str
}

/* Reads a request header. */
func (req *Request) Param(str string) string { return req.env[str] }

/* A Reader that reads from the FCGI_DATA channel from the webserver.
 * Used for file uploads, I think?
 */
func (req *Request) Data() io.Reader { return io.Reader(req.data) }

/* A Reader that reads from the FCGI_STDIN channel from the webserver.
 * This is the body of a POST in an HTTP request.
 */
func (req *Request) Stdin() io.Reader { return io.Reader(req.stdin) }

/* Private Request methods */
func newRequest(id uint16, output io.WriteCloser) *Request {
	r := new(Request)
	r.id = id
	r.env = map[string]string{}
	r.stdin = bytes.NewBuffer(make([]byte, 0))
	r.data = bytes.NewBuffer(make([]byte, 0))
	r.headers = map[string]string{}
	r.stdout = make(chan string, 1024)
	r.stderr = make(chan string, 16)
	r.responseStarted = false
	r.closeOnEnd = false
	r.startTime = time.Nanoseconds()
	r.pump_done = make(chan int)
	r.real_output = output
	// start the output pumps
	go r.pump(FCGI_STDOUT, r.stdout)
	go r.pump(FCGI_STDERR, r.stderr)
	return r
}
func (req *Request) pump(kind uint8, r chan string) {
	for { // this method reads strings from r
		// and outputs the bytes of a FCGIPacket
		s := <-r
		b := newFCGIPacketString(kind, req.id, s).bytes()
		// Log("<- : "+string(b));
		req.real_output.Write(b)
		if s == "" {
			break
		}
	}
	req.pump_done <- 1
}
func (req *Request) end(appStatus uint32, protocolStatus uint8) {
	// send the done messages
	req.abort()
	// wait for the pumps to drain
	<-req.pump_done
	<-req.pump_done
	// write the final packet
	req.real_output.Write(newFCGIPacket(FCGI_END_REQUEST, req.id, newEndRequest(appStatus, protocolStatus).bytes()).bytes())
	// if the webserver requested that we close this connection
	if req.closeOnEnd {
		// then close it
		req.real_output.Close()
	}
}
func (req *Request) startResponse(status string, headers map[string]string) {
	req.stdout <- "Status: "+status+"\r\n"
	for key, val := range headers {
		req.stdout <- key+": "+val+"\r\n"
	}
	req.stdout <- "\r\n"
	req.responseStarted = true
}
func (req *Request) handle(application Handler) {
	application(req)
	req.end(200, FCGI_REQUEST_COMPLETE)
}
func (req *Request) abort() {
	req.stdout <- ""
	req.stderr <- ""
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
	}
}

/* handleConnection is the handler that reads actual FCGI packets off the wire,
 * it fills in data on the proper request, and once the body of the request is in,
 * it dispatches the request to the specified worker pool
 */
func handleConnection(rw io.ReadWriteCloser, pool *WorkerPool) { // chan *Request, application Handler) {
	requests := map[uint16]*Request{}
	for {
		p, err := readFCGIPacket(io.Reader(rw))
		if err == os.EOF {
			break
		} else if err != nil {
			os.Stderr.WriteString(err.String() + "\r\n")
		}
		switch p.hdr.Kind {
		case FCGI_BEGIN_REQUEST:
			// TODO: since we dont read the real closeOnEnd flag down below, we can skip reading the content
			// var h FCGIBeginRequest;
			// binary.Read(bytes.NewBuffer(p.content), binary.BigEndian, &h);
			req := newRequest(p.hdr.RequestId, io.WriteCloser(rw))
			// lighttpd sets this backwards atm, so we cant use the real value:
			// req.closeOnEnd = (h.Flags == 0);
			// TODO: find a webserver that supports multiplexed fastcgi
			req.closeOnEnd = true
			// fmt.Printf("setting req.closeOnEnd = %s from %d", req.closeOnEnd, h.Flags);
			requests[p.hdr.RequestId] = req
		case FCGI_PARAMS:
			requests[p.hdr.RequestId].processParams(p.content)
		case FCGI_STDIN:
			if p.hdr.ContentLength == uint16(0) {
				// send the request into the worker pool
				pool.assignWork(requests[p.hdr.RequestId])
			} else {
				requests[p.hdr.RequestId].stdin.Write(p.content)
			}
		case FCGI_DATA:
			if p.hdr.ContentLength > uint16(0) {
				requests[p.hdr.RequestId].data.Write(p.content)
			}
		case FCGI_ABORT_REQUEST:
			requests[p.hdr.RequestId].abort()
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
	Log(fmt.Sprintf("Starting worker pool (%d)", pool_size))
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
		Log(fmt.Sprintf("Worker %d completed: %s in %.2f ms.\r\n", id, req.env["REQUEST_URI"], float64(time.Nanoseconds()-req.startTime)*float64(10e-6)))
	}
	// Log(fmt.Sprintf("Worker %d exiting\r\n", id));
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
	// r = io.Reader(NewProxyReader(r));
	hdr, err := newFCGIHeader(r)
	if err != nil {
		return nil, err
	}
	// if hdr.Version != 1 {
	// return nil, os.NewError("Invalid packet.");
	// }
	p.hdr = hdr
	if p.hdr.ContentLength > 0 {
		p.content = make([]byte, p.hdr.ContentLength+uint16(p.hdr.PaddingLength))
		_, er := r.Read(p.content)
		p.content = p.content[0:p.hdr.ContentLength] // leave the padding bytes sitting in memory?
		return p, er
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
func Log(msg string) {
	f, err := os.Open("fcgi.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		os.Stderr.WriteString("Err1: " + err.String() + "\r\n")
	}
	n, err := f.WriteString(msg)
	if err != nil {
		os.Stderr.WriteString("Err2: " + err.String() + "\r\n")
	}
	if n == 0 {
		os.Stderr.WriteString("Err3: 0 bytes written\r\n")
	}
	err = f.Close()
	if err != nil {
		os.Stderr.WriteString("Err: " + err.String() + "\r\n")
	}
	// os.Stderr.WriteString("Log: "+msg+"\r\n");
}

type ProxyReader struct {
	io.Reader
	r io.Reader
}

func NewProxyReader(r io.Reader) *ProxyReader { return &ProxyReader{r: r} }
func (self *ProxyReader) Read(b []byte) (n int, err os.Error) {
	n, err = self.r.Read(b)
	Log(fmt.Sprintf("Read: n:%d b:%s err: %s\r\n", n, b, err))
	return n, err
}
