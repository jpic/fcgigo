include ../../../Make.$(GOARCH)

TARG=fcgi
GOFILES=\
	fcgi.go\
	listener.go\
	handler.go\

include $(GOROOT)/src/Make.pkg

fmt:
	gofmt -w *.go

