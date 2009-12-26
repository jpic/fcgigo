/*
 * This is a test program that runs the "Arc Challenge", a simple dynamic form.
 */
package main

import (
	"fmt"
	"runtime"
	"fcgi"
	// "time";
)

func arc_application(req *fcgi.Request) {
	fcgi.Log("Application started")
	req.SetStatus("200 OK")
	req.Write("<html><body><form action='' method='POST'>")
	s, ok := req.Form["said"]
	if ok {
		req.Write("You said: " + s[0] + "<br>")
	}
	req.Write("Say something: <input type=text name='said'><input type=submit></form></body></html>")
	fcgi.Log("Application ended")
}

func main() int {
	runtime.GOMAXPROCS(4)
	err := fcgi.RunTCP("127.0.0.1:7134", arc_application, 1)
	if err != nil {
		fmt.Println("err in main", err.String())
		return 1
	}

	return 0
}
