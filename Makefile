include ../../../Make.$(GOARCH)

TARG=http/fcgi
GOFILES=\
	fcgi.go\
	listener.go\
	handler.go\

include $(GOROOT)/src/Make.pkg

fmt:
	gofmt -w *.go

%.out: %.go
	$(QUOTED_GOBIN)/$(GC) -I _test -o $@.$(O) $<
	$(QUOTED_GOBIN)/$(LD) -L _test -o $@ $@.$(O)
