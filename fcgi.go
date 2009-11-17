package main

import (
	"os";
	"io";
	"net";
	"fmt";
	"encoding/binary";
	"bytes";
	"runtime";
	"time";
);

var (
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
func (er *FCGIEndRequest) bytes() ([]byte) {
	buf := make([]byte, 8);
	binary.BigEndian.PutUint32(buf, er.appStatus);
	buf[4] = er.protocolStatus;
	return buf;
}

type FCGIRequest struct {
	id uint16;
	_out *net.TCPConn;
	params map[string] string;
	stdin *bytes.Buffer;
	stdout chan string;
	stderr chan string;
	responseStarted bool;
	closeOnEnd bool;
	pump_done chan int; // for signalling when a pump goroutine dies
}
func newFCGIRequest(id uint16, output *net.TCPConn) *FCGIRequest {
	r := new(FCGIRequest);
	r.id = id;
	r._out = output;
	r.params = map[string]string {};
	r.stdin = bytes.NewBuffer(make([]byte,0));
	r.stdout = make(chan string);
	r.stderr = make(chan string);
	r.responseStarted = false;
	r.closeOnEnd = false;
	r.pump_done = make(chan int);
	// start the output pumps
	go r.pump(FCGI_STDOUT, r.stdout);
	go r.pump(FCGI_STDERR, r.stderr);
	return r;
}
func (req *FCGIRequest) pump(kind uint8, r chan string) {
	for {
		b := <-r;
		bb := newFCGIPacketString(kind, req.id, b ).bytes();
		req._out.Write(bb);
		if b == "" { break }
	}
	req.pump_done <- 1;
}
func (req *FCGIRequest) start_response(status string, headers map[string] string ) {
	req.stdout <- "Status: "+status+"\r\n";
	for key, val := range headers {
		req.stdout <- key + ": " + val + "\r\n";
	}
	req.stdout <- "\r\n";
	req.responseStarted = true;
}
func (req *FCGIRequest) write(text string) {
	if req.responseStarted {
		req.stdout <- text;
	}
}
func (req *FCGIRequest) end(appStatus uint32, protocolStatus uint8) {
	req.abort();
	// wait for the pumps to drain
	<-req.pump_done;
	<-req.pump_done;
	req._out.Write(newFCGIPacket(FCGI_END_REQUEST, req.id, newEndRequest(appStatus, protocolStatus).bytes()).bytes());
}
func (req *FCGIRequest) handle() {
	// this would normally be a call out to some application handler
	// for now do a minimal wsgi act-alike
	req.start_response("200 OK", map[string] string { "Connection": "keep-alive", });
	// fmt.Printf("Request response started: %s in %f ms.\r\n", req.params["REQUEST_URI"], (ms() - start));
	req.write("Hello, WOrld!");
	// fmt.Printf("Request first write: %s in %f ms.\r\n", req.params["REQUEST_URI"], (ms() - start));
	// the req.end() should always be in here, it is outside the WSGI spec
	req.end(200, FCGI_REQUEST_COMPLETE);
	// fmt.Println("FCGIRequest.handle() done.");
}
func (req *FCGIRequest) abort() {
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
func (req *FCGIRequest) processParams(text []byte) {
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

func handle(rw *net.TCPConn) {
	requests := map[uint16] *FCGIRequest {};
	start := int64(0);
	for {
		p, err := readFCGIPacket(rw);
		if err != nil { // EOF is normal error here
			break;
		}
		switch p.hdr.Kind {
			case FCGI_BEGIN_REQUEST:
				var h FCGIBeginRequest;
				start = time.Nanoseconds();
				binary.Read(bytes.NewBuffer(p.content), binary.BigEndian, &h);
				req := newFCGIRequest(p.hdr.RequestId, rw);
				req.closeOnEnd = (h.Flags == 0);
				requests[p.hdr.RequestId] = req;
			case FCGI_PARAMS:
				requests[p.hdr.RequestId].processParams(p.content);
			case FCGI_STDIN:
				if p.hdr.ContentLength == uint16(0) {
					// because of a bug, Close() can't happen inside a goroutine other than the one Read()ing it
					// so once we dispatch the request, no other FCGI records will be processed
					// this means that multiplexing, and ABORT_REQUEST, will not work (not that any webservers support it now anyway)
					req := requests[p.hdr.RequestId];
					req.handle();
					fmt.Printf("Request complete: %s in %.2f ms.\r\n", req.params["REQUEST_URI"], float64(time.Nanoseconds() - start) * float64(10e-6));
					if req.closeOnEnd {
						rw.Close();
						break;
					}
				} else {
					requests[p.hdr.RequestId].stdin.Write(p.content);
				}
			case FCGI_ABORT_REQUEST:
				requests[p.hdr.RequestId].abort();
		}
	}
	rw.Close();
}

func listen() os.Error {
	addr, e := net.ResolveTCPAddr("127.0.0.1:7143");
	if e != nil { return e }
	s, e := net.ListenTCP("tcp4", addr);
	if e != nil { return e }
	fmt.Println("Listening");
	for {
		rw, e := s.AcceptTCP();
		if e != nil {
			fmt.Printf("Accept error: %s\r\n", e);
			break;
		}
		fmt.Println("Connection accepted");
		go handle(rw);
	}
	s.Close();
	return nil;
}

func main() int {
	runtime.GOMAXPROCS(4);
	err := listen();
	if err != nil {
		fmt.Println("err in main",err.String());
		return 1;
	}

	return 0;
}

