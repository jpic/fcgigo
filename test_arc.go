/*
 * This is a test program that runs the "Arc Challenge", a simple dynamic form.
 * It isn't quite right yet, because there is no persistent storage.
 */
package main

import (
	"fcgi"
)


func main() int {
	fcgi.Run(func(req *fcgi.Request) {
		req.Write("<html><body><form action='' method='POST'>")
		if s, ok := req.Form["said"]; ok {
			req.Write("You said: " + s[0] + "<br>")
		}
		req.Write("<input type=text name='said'><input type=submit></form></body></html>")
	},
		100)
	return 0
}
