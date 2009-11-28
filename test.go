package main

import (
	"fmt";
	"runtime";
	"fcgi";
);

func hello_application(req *fcgi.Request, start_response func(string,map[string] string)) {
	start_response("200 OK", map[string] string { "Connection": "keep-alive", });
	req.Write("Hello, WOrld!");
}

func main() int {
	runtime.GOMAXPROCS(4);
	err := fcgi.ServeTCP("127.0.0.1:7143", hello_application);
	if err != nil {
		fmt.Println("err in main",err.String());
		return 1;
	}

	return 0;
}

