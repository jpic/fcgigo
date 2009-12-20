package main

import (
	"fmt";
	"runtime";
	"fcgi";
	// "time";
);

func hello_application(req *fcgi.Request ) {
	req.Status("200 OK");
	req.Header("Connection", "keep-alive");
	req.Write("Hello, WOrld!");
}

func main() int {
	runtime.GOMAXPROCS(4);
	err := fcgi.ServeTCP("127.0.0.1:7143", hello_application, 100);
	if err != nil {
		fmt.Println("err in main",err.String());
		return 1;
	}

	return 0;
}

