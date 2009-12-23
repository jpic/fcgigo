
all: fcgi.a test_tcp.out test_run.out

test_tcp.out: fcgi.a
test_run.out: fcgi.a

%.8: %.go
	8g -I . $<

%.out: %.8
	8l -o $@ $<

%.a: %.8
	gopack grc $@ $<
