/* This is a test program suitable for being launched by a webserver.
 * Example lighttpd config:
fastcgi.server =("" =>
									("" =>
										(
  "bin-path" => "/opt/go/code/fcgi/test_dyn.out",
											"port" => 7134,
											"check-local" => "disable",
											"max-procs" => 1,
										),
									)
)
*/

package main

import (
	"fmt";
	"runtime";
	"fcgi";
	"os";
);

func hello_application(req *fcgi.Request ) {
	req.Status("200 OK");
	req.Header("Connection", "keep-alive");
	req.Write("Hello, WOrld!");
}

func main() int {
	runtime.GOMAXPROCS(4);
	err := fcgi.Run(hello_application, 10);
	if err != nil {
		os.Stderr.WriteString(fmt.Sprintf("err in main: %s",err.String()));
		return 1;
	}
	return 0;
}

