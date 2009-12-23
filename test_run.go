package main

import (
	"fmt";
	"runtime";
	"fcgi";
	"os";
	// "time";
);

func hello_application(req *fcgi.Request ) {
	req.Status("200 OK");
	req.Header("Connection", "keep-alive");
	req.Write("Hello, WOrld!");
}

func main() int {
	fcgi.Log("main starting\r\n");
	runtime.GOMAXPROCS(4);
	err := fcgi.Run(hello_application, 10);
	if err != nil {
		os.Stderr.WriteString(fmt.Sprintf("err in main: %s",err.String()));
		return 1;
	}
	return 0;
}

