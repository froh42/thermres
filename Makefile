BINS = thermres thermres-plot
BINDIR = $(HOME)/.local/bin

all: $(BINS)

thermres:
	go build -o $@ .

thermres-plot:
	go build -o $@ ./cmd/thermres-plot

install: all
	mkdir -p $(BINDIR)
	install -m 755 thermres-plot thermres $(BINDIR)/
	sudo chown root $(BINDIR)/thermres
	sudo chmod u+s $(BINDIR)/thermres

uninstall:
	rm -f $(addprefix $(BINDIR)/,$(BINS))

clean:
	rm -f $(BINS)

.PHONY: all install uninstall clean
