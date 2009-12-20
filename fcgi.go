package fcgi

import (
	"os";
	"io";
	"net";
	"fmt";
	"encoding/binary";
	"bytes";
	"time";
);

var ( // from fcgi.h
	FCGI_REQUEST_COMPLETE =    uint8(0);
	FCGI_BEGIN_REQUEST =       uint8(1);
	FCGI_ABORT_REQUEST =       uint8(2);
	FCGI_END_REQUEST =         uint8(3);
	FCGI_PARAMS =              uint8(4);
	FCGI_STDIN =               uint8(5);
	FCGI_STDOUT =              uint8(6);
	FCGI_STDERR =              uint8(7);
	FCGI_DATA =                uint8(8);
	FCGI_GET_VALUES =          uint8(9);
	FCGI_GET_VALUES_RESULT =  uint8(10);
	FCGI_UNKNOWN_TYPE =       uint8(11);
	FCGI_MAXTYPE = (FCGI_UNKNOWN_TYPE);
);

/* The type for a callable that handles requests */
type Handler func( *Request );

type FCGIHeader struct {
	Version uint8;
	Kind uint8;
	RequestId uint16;
	ContentLength uint16;
	PaddingLength uint8;
	Reserved uint8;
}
func newFCGIHeader(r io.Reader) (FCGIHeader, os.Error) {
	var h FCGIHeader;
	err := binary.Read(r, binary.BigEndian, &h);
	return h, err;
}
func (h *FCGIHeader) bytes() ([]byte) {
	order := binary.BigEndian;
	buf := make([]byte, 8);
	buf[0] = h.Version;
	buf[1] = h.Kind;
	order.PutUint16(buf[2:4],h.RequestId);
	order.PutUint16(buf[4:6],h.ContentLength);
	buf[6] = h.PaddingLength;
	buf[7] = h.Reserved;
	return buf;
}

type FCGIPacket struct {
	hdr FCGIHeader;
	content []byte;
}
func readFCGIPacket(r io.Reader) (*FCGIPacket, os.Error) {
	p := new(FCGIPacket);
	hdr, err := newFCGIHeader(r);
	if err != nil {
		return nil, err;
	}
	p.hdr = hdr;
	if p.hdr.ContentLength > 0 {
		p.content = make([]byte, p.hdr.ContentLength + uint16(p.hdr.PaddingLength));
		_, er := r.Read(p.content);
		p.content = p.content[0:p.hdr.ContentLength]; // leave the padding bytes sitting in memory?
		return p, er;
	}
	return p, nil;
}
func newFCGIPacket(kind uint8, reqid uint16, content []byte) (*FCGIPacket) {
	p := new(FCGIPacket);
	p.hdr = *new(FCGIHeader);
	p.hdr.Version = 1;
	p.hdr.Kind = kind;
	p.hdr.RequestId = reqid;
	p.content = content;
	p.hdr.ContentLength = uint16(len(p.content));
	p.hdr.PaddingLength = uint8(-p.hdr.ContentLength & 7);
	return p;
}
func newFCGIPacketString(kind uint8, reqid uint16, content string) (*FCGIPacket) {
	buf := bytes.NewBufferString(content);
	return newFCGIPacket(kind, reqid, buf.Bytes());
}
func (p *FCGIPacket) bytes() ([]byte) {
	buf := bytes.NewBuffer(p.hdr.bytes());
	buf.Write(p.content);
	for i := uint8(0); i < p.hdr.PaddingLength; i++ {
		buf.WriteByte(0);
	}
	return buf.Bytes();
}

type FCGIBeginRequest struct {
	Role uint16;
	Flags uint8;
	Reserved [5]uint8;
}
type FCGIEndRequest struct {
	appStatus uint32;
	protocolStatus uint8;
	reserved [3]uint8;
}
func newEndRequest(appStatus uint32, protocolStatus uint8) (*FCGIEndRequest) {
	er := new(FCGIEndRequest);
	er.appStatus = appStatus;
	er.protocolStatus = protocolStatus;
	return er;
}
func (self *FCGIEndRequest) bytes() ([]byte) {
	buf := make([]byte, 8);
	binary.BigEndian.PutUint32(buf, self.appStatus);
	buf[4] = self.protocolStatus;
	return buf;
}

type Request struct {
	id uint16;
	_out io.WriteCloser;
	params map[string] string; // from the request
	headers map[string] string; // for the response
	status string; // for the response
	stdin *bytes.Buffer;
	data *bytes.Buffer;
	stdout chan string;
	stderr chan string;
	responseStarted bool;
	startTime int64;
	closeOnEnd bool;
	pump_done chan int; // for signalling when a pump goroutine dies
}


func newRequest(id uint16, output io.WriteCloser) *Request {
	r := new(Request);
	r.id = id;
	r._out = output;
	r.params = map[string]string {};
	r.headers = map[string]string {};
	r.stdin = bytes.NewBuffer(make([]byte,0));
	r.data = bytes.NewBuffer(make([]byte,0));
	r.stdout = make(chan string, 1024);
	r.stderr = make(chan string, 16);
	r.responseStarted = false;
	r.closeOnEnd = false;
	r.startTime = time.Nanoseconds();
	r.pump_done = make(chan int);
	// start the output pumps
	go r.pump(FCGI_STDOUT, r.stdout);
	go r.pump(FCGI_STDERR, r.stderr);
	return r;
}
/* Public Request Methods */
func (req *Request) Status(status string) {
	if ! req.responseStarted {
		req.status = status;
	}
}
func (req *Request) Header(str string, val string) {
	if ! req.responseStarted {
		req.headers[str] = val;
	}
}
func (req *Request) Error(str string) {
	if ! req.responseStarted {
		req.startResponse("500 Application Error", req.headers);
	}
	req.stderr <- str;
}
func (req *Request) Write(str string) {
	if ! req.responseStarted {
		req.startResponse(req.status, req.headers);
	}
	req.stdout <- str;
}
/* private Request methods */
func (req *Request) pump(kind uint8, r chan string) {
	for { // this method reads strings from r
		// and outputs the bytes of a FCGIPacket
		s := <-r;
		b := newFCGIPacketString(kind, req.id, s ).bytes();
		req._out.Write(b);
		if s == "" { break }
	}
	req.pump_done <- 1;
}
func (req *Request) end(appStatus uint32, protocolStatus uint8) {
	// send the done messages
	req.abort();
	// wait for the pumps to drain
	<-req.pump_done;
	<-req.pump_done;
	// write the final packet
	req._out.Write(newFCGIPacket(FCGI_END_REQUEST, req.id, newEndRequest(appStatus, protocolStatus).bytes()).bytes());
	// if the webserver requested that we close this connection
	if req.closeOnEnd {
		// then close it
		req._out.Close();
	}
}
func (req *Request) startResponse(status string, headers map[string] string ) {
	req.stdout <- "Status: "+status+"\r\n";
	for key, val := range headers {
		req.stdout <- key + ": " + val + "\r\n";
	}
	req.stdout <- "\r\n";
	req.responseStarted = true;
}
func (req *Request) handle(application Handler) {
	application(req);
	req.end(200, FCGI_REQUEST_COMPLETE);
}
func (req *Request) abort() {
	req.stdout <- "";
	req.stderr <- "";
}

func getOneSize (slice []byte) (uint32, []byte) {
	size := slice[0];
	r := 1;
	if size >> 7 == 1 {
		er := binary.Read(bytes.NewBuffer(slice[0:4]), binary.BigEndian, &size);
		if er != nil {
			fmt.Printf("binary.Read error: %s\r\n", er);
			return 0, slice[len(slice):len(slice)];
		}
		r = 4;
	}
	return uint32(size), slice[r:len(slice)];
};
func getOneValue (slice []byte, size uint32) (string, []byte) {
	return string(slice[0:size]), slice[size:len(slice)];
};
func (req *Request) processParams(text []byte) {
	// FastCGI uses it's own key,value pair encoding
	// processParams reads an encoded []byte into this
	// request's params map
	slice := text[0:len(text)];
	for len(slice) > 0 {
		var (
			key_len uint32 = 0;
			val_len uint32 = 0;
			key string = "";
			val string = "";
		);
		key_len, slice = getOneSize(slice);
		val_len, slice = getOneSize(slice);
		key, slice = getOneValue(slice, key_len);
		val, slice = getOneValue(slice, val_len);
		req.params[key] = val;
	}
}

func fcgi_slave(rw io.ReadWriteCloser, pool chan *Request, application Handler) {
	requests := map[uint16] *Request {};
	for {
		p, err := readFCGIPacket(io.Reader(rw));
		if err != nil { // EOF is normal error here
			break;
		}
		switch p.hdr.Kind {
			case FCGI_BEGIN_REQUEST:
				// TODO: since we dont read the real closeOnEnd flag down below, we can skip reading the content
				// var h FCGIBeginRequest;
				// binary.Read(bytes.NewBuffer(p.content), binary.BigEndian, &h);
				req := newRequest(p.hdr.RequestId, io.WriteCloser(rw));
				// lighttpd sets this backwards atm, so we cant user the real value: 
				// req.closeOnEnd = (h.Flags == 0);
				// TODO: find a webserver that supports multiplexed fastcgi
				req.closeOnEnd = true;
				// fmt.Printf("setting req.closeOnEnd = %s from %d", req.closeOnEnd, h.Flags);
				requests[p.hdr.RequestId] = req;
			case FCGI_PARAMS:
				requests[p.hdr.RequestId].processParams(p.content);
			case FCGI_STDIN:
				if p.hdr.ContentLength == uint16(0) {
					// there are two ways this can be done here
					// 1) put the request into a pool of workers
					pool <- requests[p.hdr.RequestId];
					// or 2) call the handler directly
					// e.g., requests[p.hdr.RequestId].handle(application);
					// or 2a) call the handler as a goroutine
					// only 1) and 2a) would support multiplexing
				} else {
					requests[p.hdr.RequestId].stdin.Write(p.content);
				}
			case FCGI_DATA:
				if p.hdr.ContentLength > uint16(0) {
					requests[p.hdr.RequestId].data.Write(p.content);
				}
			case FCGI_ABORT_REQUEST:
				requests[p.hdr.RequestId].abort();
		}
	}
	rw.Close();
}

func worker (id int, workchan chan *Request, workdone chan int, application Handler) {
	// each worker continually
	for {
		// reads from their work channel
		req := <-workchan;
		// breaks on a close signal (a nil value)
		if req == nil { break; }
		// handles the request
		req.handle(application);
		fmt.Printf("Worker %d completed: %s in %.2f ms.\r\n", id, req.params["REQUEST_URI"], float64(time.Nanoseconds() - req.startTime) * float64(10e-6));
	}
	fmt.Printf("Worker %d exiting\r\n", id);
	// when finished, notify
	workdone <-1
};

/** ServeTCP
*	Creates a FastCGI Responder that listens on the supplied address.  This functions runs forever.
*	addr - a string like "localhost:1234", or "0.0.0.0:999", that specifies a local interface to listen on.
*	application - the callable which will produce the output for each Request.
**/
func ServeTCP(addr string, application Handler, pool_size int) os.Error {
	a, e := net.ResolveTCPAddr(addr);
	if e != nil { return e }
	s, e := net.ListenTCP("tcp4", a);
	if e != nil { return e }
	fmt.Println("Listening");
	fmt.Println("Starting goroutine workers...");
	workchan := make(chan *Request, pool_size);
	workdone := make(chan int);
	for i := 0; i < pool_size; i++ {
		// spawn a worker goroutine into the pool
		go worker(i, workchan, workdone, application);
	}

	for {
		rw, e := s.AcceptTCP();
		if e != nil {
			fmt.Printf("Accept error: %s\r\n", e);
			break;
		}
		// fmt.Println("Connection accepted");
		go fcgi_slave(io.ReadWriteCloser(rw), workchan, application);
	}

	for i := 0; i < pool_size; i++ {
		workchan <- nil; // send a close signal
		<-workdone; // wait for ack
	}
	s.Close();
	return nil;
}

/** ServeFD
*	Creates a FastCGI Responder on an already open FD.  This is how one would support being spawned dynamically.
*	ATM, I don't know how to build a ReadWriter out of a raw fd, so this is here for documentation purposes.
*	fd - the fd to communicate over, typically defined by FCGI_LISTENSOCK_FILENO (0)
*	application - the callable which will produce the output for each Request.
**/
func ServeFD(fd int, application Handler) os.Error { // need to find a way to create a socket from a raw fd first
	return nil;
}

