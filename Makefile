
include $(GOROOT)/src/Make.$(GOARCH)

TARG=fcgi
GOFILES=\
	fcgi.go\

include $(GOROOT)/src/Make.pkg

fmt:
	gofmt -w *.go

