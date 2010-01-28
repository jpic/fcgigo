package fcgi

import (
	"fmt"
	"io"
	"bufio"
	"os"
	"encoding/binary"
	"strings"
	"bytes"
	"http"
	"net"
	"testing/iotest"
)

type WebServer struct {
	listenAddress  string
	listenPort     int
	responders     []*connection
	nextResponseId int
}

func NewWebServer(addr string, responders []string) *WebServer {
	server := &WebServer{
		listenAddress: addr,
		responders: make([]*connection, len(responders)),
		nextResponseId: -1,
	}
	Log("WebServer: Connecting to responders")
	for i, addr := range responders {
		server.responders[i] = dialResponder(addr)
	}
	return server
}
func (self *WebServer) getNextResponder() *connection {
	self.nextResponseId = (self.nextResponseId + 1) % len(self.responders)
	return self.responders[self.nextResponseId]
}
func (self *WebServer) disconnectAllResponders() {
	for i, responder := range self.responders {
		responder.socket.Close()
		self.responders[i] = nil
	}
}

func fcgiWrite(w io.Writer, kind uint8, id RequestId, content []byte) {
	p := newFCGIPacket(kind, id, content)
	Log("WebServer: fcgiWrite packet:", p)
	w.Write(p.bytes())
}


func (self *WebServer) Run(ready chan bool, exit chan bool, done chan bool) (err os.Error) {
	defer Done(done)
	Log("WebServer: Binding listen socket")
	// open our listen socket
	if laddr, err := net.ResolveTCPAddr(self.listenAddress); err == nil {
		self.listenPort = laddr.Port
		if lsock, err := net.ListenTCP("tcp4", laddr); err == nil {
			accept := AcceptChannel(lsock)
			ready <- true
			for {
				// accept a new connection, or die on request
				select {
				case sock := <-accept:
					if sock != nil {
						Log("WebServer: Connection accepted.")
						// spawn a handler for the new connetion
						go func() {
							// read http requests from this socket
							for {
								if h, err := http.ReadRequest(bufio.NewReader(sock)); err == nil {
									// dispatch it to a responder
									self.getNextResponder().sendRequest(h, sock, self) // the responder will reply asynchronously on the socket
									// this connection won't read another http request until after the FCGI request is done being sent
									// but multiple connections can still multiplex requests across responders
								} else {
									Log("WebServer: Error reading HTTP Request from client. %s", err.String())
								}
							}
						}()
					}
				case <-exit:
					break
				}
			}
			lsock.Close()
		} else {
			Log("WebServer: failed to listen", err)
		}
	} else {
		Log("WebServer: invalid listen address", err)
	}
	return err
}

type connection struct {
	hostname      string
	socket        Socket
	requests      []Socket
	nextRequestId RequestId
}

func dialResponder(addr string) (ret *connection) {
	laddr, _ := net.ResolveTCPAddr("127.0.0.1:0")
	if raddr, err := net.ResolveTCPAddr(addr); err == nil {
		if sock, err := net.DialTCP("tcp4", laddr, raddr); err == nil {
			ret = &connection{
				hostname: addr,
				socket: Socket(sock),
				requests: make([]Socket, 65535), // request ids in fastcgi are uint16
				nextRequestId: 1,
			}
			go ret.readAllPackets()
			Log("WebServer: connected to responder.")
		} else {
			Log("DialTCP failed:", err.String())
		}
	} else {
		Log("ResolveTCPAddr failed:", err.String())
	}
	return ret
}
func (self *connection) getNextRequestId() RequestId {
	for i := self.nextRequestId; int(i) < len(self.requests); i++ {
		if self.requests[i] == nil {
			self.nextRequestId = i + 1
			return i
		}
	}
	return 0
}
func (self *connection) readAllPackets() {
	// launched as a go-routine
	for {
		if p, err := ReadFCGI(self.socket); err == nil { // read a FastCGI packet
			if self.requests[p.hdr.ReqId] == nil {
				Log("WebServer: Discarding packet from FCGI connection with an unknown RequestId: %d", p.hdr.ReqId)
				continue
			}
			switch p.hdr.Kind {
			case FCGI_STDOUT:
				Log("WebServer: got STDOUT: %s", p.content)
				self.requests[p.hdr.ReqId].Write(p.content)
			case FCGI_STDERR:
				Log("WebServer: got STDERR: %s", p.content)
			case FCGI_END_REQUEST:
				Log("WebServer: got END_REQUEST")
				self.requests[p.hdr.ReqId] = nil
				self.nextRequestId = p.hdr.ReqId // note that this is free
			default:
				Log("WebServer: responder sent unknown packet type:", p.hdr.Kind)
			}
		}
	}
}
func (self *connection) sendRequest(req *http.Request, reply Socket, from *WebServer) {
	id := self.getNextRequestId()
	Log("WebServer: choosing request id", id, "in", self.requests[0:5])
	self.requests[id] = reply
	Log("WebServer: request read, building params")
	// to be sent over FCGI_PARAMS
	params := map[string]string{
		"SERVER_SOFTWARE": "fcgigo-server",
		"HTTP_HOST": req.Host,
		"SERVER_NAME": req.Host,
		"REQUEST_URI": req.RawURL,
		"REQUEST_METHOD": req.Method,
		"GATEWAY_INTERFACE": "FCGI/1.0",
		"SERVER_PORT": fmt.Sprintf("%d", from.listenPort),
		"SERVER_ADDR": "127.0.0.1",
		"SERVER_PROTOCOL": req.Proto,
		"REMOTE_PORT": "0",
		"REMOTE_ADDR": "",
		"SCRIPT_NAME": "",
		"PATH_INFO": "",
		"PATH_TRANSLATED": "",
		"SCRIPT_FILENAME": "",
		"DOCUMENT_ROOT": "",
		"QUERY_STRING": req.URL.RawQuery,
	}

	// set the real document_root
	if dir, err := os.Getwd(); err != nil {
		params["DOCUMENT_ROOT"] = dir
	}

	// patch the ?query_string to include the #fragment
	if len(req.URL.Fragment) > 0 {
		Log("WebServer: adding fragment to query string")
		params["QUERY_STRING"] = params["QUERY_STRING"] + "#" + req.URL.Fragment
	}

	// carry over the content-length
	if c, ok := req.Header["Content-Length"]; ok {
		Log("WebServer: Setting content-length")
		params["CONTENT_LENGTH"] = c
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
		params[k] = v
	}

	Log("WebServer: sending BEGIN_REQUEST")
	/* Send a FCGI_BEGIN_REQUEST */
	fcgiWrite(self.socket, FCGI_BEGIN_REQUEST, id, newFCGIBeginRequest(true).bytes())

	/* Encode and Send the FCGI_PARAMS */
	encodeSize := func(size int) []byte {
		if size > 127 {
			buf := make([]byte, 4)
			binary.BigEndian.PutUint32(buf, uint32(size))
			return buf
		}
		buf := make([]byte, 1)
		buf[0] = uint8(size)
		return buf
	}

	buf := bytes.NewBuffer(make([]byte, 0, 4096))
	for k, v := range params {
		nk := len(k)
		nv := len(v)
		// Log("WebServer: FCGI_PARAM:", encodeSize(nk), k, encodeSize(nv), v)
		buf.Write(encodeSize(nk))
		buf.Write(encodeSize(nv))
		buf.WriteString(k)
		buf.WriteString(v)
	}
	Log("WebServer: sending FCGI_PARAMS")
	fcgiWrite(self.socket, FCGI_PARAMS, id, buf.Bytes())

	/* Now write the STDIN from the Body of the request */
	stdin := make([]byte, FCGI_MAX_WRITE)
	for req.Body != nil {
		n, err := req.Body.Read(stdin)
		if n == 0 || err == os.EOF {
			break
		} else if err != nil {
			fmt.Println("Error in sendToResponder:", err)
			break
		} else {
			Log("WebServer: sending FCGI_STDIN")
			fcgiWrite(self.socket, FCGI_STDIN, id, stdin[0:n])
		}
	}

	// write the close messages
	Log("WebServer: sending FCGI_STDIN")
	fcgiWrite(self.socket, FCGI_STDIN, id, []byte{})

	Log("WebServer: sending FCGI_DATA")
	/* Send the FCGI_DATA (none for now) */
	fcgiWrite(self.socket, FCGI_DATA, id, []byte{})
	Log("WebServer: request send complete.")
	return
}

type ReadWriteCloserLogger struct {
	prefix string
	rwc    io.ReadWriteCloser
	r      io.Reader
	w      io.Writer
}

func NewReadWriteCloserLogger(prefix string, rwc io.ReadWriteCloser) *ReadWriteCloserLogger {
	return &ReadWriteCloserLogger{
		prefix: prefix,
		rwc: rwc,
		r: iotest.NewReadLogger(prefix, rwc),
		w: iotest.NewWriteLogger(prefix, rwc),
	}
}
func (self *ReadWriteCloserLogger) Read(p []byte) (int, os.Error) {
	return self.r.Read(p)
}
func (self *ReadWriteCloserLogger) Write(p []byte) (int, os.Error) {
	return self.w.Write(p)
}
func (self *ReadWriteCloserLogger) Close() os.Error {
	return self.rwc.Close()
}
