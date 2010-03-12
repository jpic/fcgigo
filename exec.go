package fcgi

import (
	"os"
	"io"
	"path"
	"strconv"
	"syscall"
	"fmt"
)

// dialExec will ForkExec a new process, and returns a wsConn that connects to its stdin
func dialExec(binpath string) (rwc io.ReadWriteCloser, err os.Error) {
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
	var fd, cfd, errno int
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
	// DEBUG: for now, give the new process our stdout, and stderr, but the spec says these should be closed
	// in reality, we should redirect, capture, and possibly log separately
	if _, errno = syscall.ForkExec(file, []string{}, []string{}, dir, []int{fd, 1, 2}); errno != 0 {
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
	rwc = os.NewFile(cfd, "exec://"+binpath)
	return rwc, nil
}
