/* This is test program that launches a stand-alone FastCGI Responder, listening on a TCP port.
 * Example lighttpd configuration:
fastcgi.server =("" =>
  ("" =>
    (
      "host" => "127.0.0.1",
      "port" => 7134,
      "check-local" => "disable",
    ),
  )
)

Then, you are responsible for starting this process (external to the webserver).
*/
package main

import (
	"fmt"
	"runtime"
	"fcgi"
	// "time";
)

func hello_application(req *fcgi.Request) {
	req.SetStatus("200 OK")
	req.SetHeader("Connection", "keep-alive")
	req.Write("Hello, WOrld!")
}

func main() int {
	runtime.GOMAXPROCS(4)
	err := fcgi.RunTCP("127.0.0.1:7134", hello_application, 10)
	if err != nil {
		fmt.Println("err in main", err.String())
		return 1
	}

	return 0
}
