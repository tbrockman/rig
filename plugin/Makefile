# The @joinezco/rig plugin: built here, signed here, and embedded in rig
# from ../internal/proxy/plugin. `make` at the repo root does not need any of
# this; the signed component is committed, so only a change to the plugin
# needs the Rust toolchain (rustup target add wasm32-wasip2) and wasmsign2.
.DEFAULT_GOAL := all
.DELETE_ON_ERROR:

CRATE   := rig_witmproxy_plugin
BUILT   := target/wasm32-wasip2/release/$(CRATE).wasm
OUT     := ../internal/proxy/plugin/rig.signed.wasm
SOURCES := Cargo.toml $(wildcard src/*.rs) $(shell find wit -type f)

all: $(OUT)

wasmsign:
	@command -v wasmsign2 >/dev/null 2>&1 || cargo install wasmsign2-cli

# Grouped target (&:): one keygen writes both files, so losing either regenerates
# the pair. Order-only (|) so the tool check never marks the keys stale: the
# public key is compiled into the component, so regenerating the pair changes
# the plugin's identity. key.secret stays out of git; key.public is in it.
key.secret key.public &: | wasmsign
	wasmsign2 keygen --public-key key.public --secret-key key.secret

$(BUILT): $(SOURCES) key.public
	cargo build --release --target wasm32-wasip2

$(OUT): $(BUILT) key.secret | wasmsign
	wasmsign2 sign -i $(BUILT) -o $(OUT) -k key.secret

# The rules are plain Rust; the component is not run here.
test:
	cargo test

clean:
	cargo clean

.PHONY: all wasmsign test clean
