
all: fcgi.out

%.8: %.go
	8g $<

%.out: %.8
	8l -o $@ $<

