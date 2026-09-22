GO ?= go
BIN ?= gnoshot
ROOT ?= ./testdata/cache

.PHONY: test
test:
	$(GO) test ./...

.PHONY: run
run:
	$(GO) run . serve -root $(ROOT) -sweep 0

.PHONY: install
install:
	$(GO) install .

.PHONY: lint
lint:
	$(GO) vet ./...
	gofmt -l . | tee /dev/stderr | (! read)

# Regenerate the selector table in the README against live gnoweb. The table is
# a claim about somebody else's templates, so it is checked rather than trusted.
.PHONY: selectors
selectors:
	$(GO) run . resolve \
		https://gno.land/r/gov/dao \
		https://gno.land/r/moul/config/v0 \
		https://gno.land/r/moul/config \
		https://gno.land/p/moul/txlink \
		https://gno.land/u/moul \
		https://gno.land/r/g1r6luttvrkksxh9h4asjq2qd8zsd5nlkrzvyjur/pixelgnomes \
		https://gno.land/r/does/not/exist12345

.PHONY: clean
clean:
	rm -rf $(ROOT) $(BIN)
