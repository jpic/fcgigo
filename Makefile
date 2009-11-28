
all: fcgi.a test.out 

test.out: fcgi.a

%.8: %.go
	8g -I . $<

%.out: %.8
	8l -o $@ $<

%.a: %.8
	gopack grc $@ $<
