BINS = thermres thermres-plot
BINDIR = $(HOME)/.local/bin
UNIT_DIR = $(HOME)/.config/systemd/user
UNIT = thermres.service

SRCS = $(wildcard *.go)
PLOT_SRCS = $(wildcard cmd/thermres-plot/*.go)

all: $(BINS)

thermres: $(SRCS)
	CGO_ENABLED=0 go build -o $@ .

thermres-plot: $(PLOT_SRCS)
	CGO_ENABLED=0 go build -o $@ ./cmd/thermres-plot

install: all
	mkdir -p $(BINDIR)
	install -m 755 thermres-plot thermres $(BINDIR)/
	sudo chown root $(BINDIR)/thermres
	sudo chmod u+s $(BINDIR)/thermres
	mkdir -p $(UNIT_DIR)
	install -m 644 $(UNIT) $(UNIT_DIR)/$(UNIT)
	systemctl --user daemon-reload
	systemctl --user enable --now thermres.service || true
	systemctl --user restart thermres.service
	systemctl --user status thermres.service --no-pager

uninstall:
	-systemctl --user disable --now thermres.service
	rm -f $(addprefix $(BINDIR)/,$(BINS))
	rm -f $(UNIT_DIR)/$(UNIT)
	systemctl --user daemon-reload

clean:
	rm -f $(BINS)

.PHONY: all install uninstall clean
