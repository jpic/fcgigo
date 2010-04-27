# Copyright 2010 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

include $(GOROOT)/src/Make.$(GOARCH)

TARG=http/fcgi
GOFILES=\
	fcgi.go\
	listener.go\
	handler.go\
	exec.go\

include $(GOROOT)/src/Make.pkg

fmt:
	gofmt -w *.go

_test/listener_test_exec.out: listener_test_exec.go
	$(QUOTED_GOBIN)/$(GC) -I _test -o $@.$(O) $<
	$(QUOTED_GOBIN)/$(LD) -L _test -o $@ $@.$(O)
