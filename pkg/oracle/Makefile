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

# scripts/build_lib.sh limits the library's exports to the ADBC entrypoints
# (the export lists are exports_*.{map,txt,def} in this directory).
$(LIB):
	../../scripts/build_lib.sh pkg/oracle/$(LIB)

clean:
	rm -f $(LIB) libadbc_driver_oracle.h
