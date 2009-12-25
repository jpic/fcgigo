
all: fcgi.a test_tcp.out test_run.out test_arc.out

%.8: %.go
	8g -I . $<

%.out: %.8 fcgi.a
	8l -o $@ $<

%.a: %.8
	gopack grc $@ $<
