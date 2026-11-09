# Build helper for the cgo c-shared wrapper. The Python wheel build
# expects libadbc_driver_oracle.{so,dylib,dll} produced by this Makefile.
#
# Targets:
#   make            — build the shared library for the current platform
#   make clean      — remove the shared library

ifeq ($(shell go env GOOS),linux)
	SUFFIX := so
else ifeq ($(shell go env GOOS),windows)
	SUFFIX := dll
else
	SUFFIX := dylib
endif

LIB := libadbc_driver_oracle.$(SUFFIX)

# Always run `go build`: the library depends on every package under driver/ and
# internal/, and Go's build cache already skips the work when nothing changed. (A
# prerequisite list of the files in this directory left stale libraries behind.)
.PHONY: all clean $(LIB)
all: $(LIB)

$(LIB):
	cd ../.. && go build -buildmode=c-shared -tags driverlib \
		-o pkg/oracle/$(LIB) ./pkg/oracle

clean:
	rm -f $(LIB) libadbc_driver_oracle.h
