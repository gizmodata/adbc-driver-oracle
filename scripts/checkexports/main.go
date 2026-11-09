// Command checkexports checks a c-shared ADBC driver library against the
// ADBC shared-library requirements
// (https://adbc-drivers.org/building-drivers/shared-library-requirements.html):
// it must export AdbcDriverInit and the entrypoint the driver manager derives
// from the file name (libadbc_driver_foo.so -> AdbcDriverFooInit), and
// nothing that does not begin with "Adbc" except the names given with -allow.
//
// It reads ELF, Mach-O and PE files with the standard library, so the same
// check runs on every CI runner:
//
//	go run ./scripts/checkexports -allow OracleDriverInit pkg/oracle/libadbc_driver_oracle.so
package main

import (
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

func main() {
	allow := flag.String("allow", "", "comma-separated exported names allowed without the Adbc prefix")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: checkexports [-allow a,b] <library>")
		os.Exit(2)
	}
	path := flag.Arg(0)
	syms, err := exports(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "checkexports: %s: %v\n", path, err)
		os.Exit(2)
	}
	allowed := map[string]bool{}
	for _, a := range strings.Split(*allow, ",") {
		if a != "" {
			allowed[a] = true
		}
	}
	have := map[string]bool{}
	var extra []string
	for _, s := range syms {
		have[s] = true
		if !strings.HasPrefix(s, "Adbc") && !allowed[s] {
			extra = append(extra, s)
		}
	}
	var problems []string
	for _, want := range []string{"AdbcDriverInit", entrypoint(path)} {
		if !have[want] {
			problems = append(problems, "missing required export "+want)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		if len(extra) > 10 {
			extra = append(extra[:10], fmt.Sprintf("... (%d more)", len(extra)-10))
		}
		problems = append(problems, "exports names without the Adbc prefix: "+strings.Join(extra, ", "))
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "checkexports: %s: %s\n", filepath.Base(path), p)
		}
		os.Exit(1)
	}
	fmt.Printf("%s: %d exports, all Adbc* (allowed extras: %v); %s and AdbcDriverInit present\n",
		filepath.Base(path), len(syms), keys(allowed), entrypoint(path))
}

// entrypoint mirrors the driver manager's derivation
// (AdbcDriverManagerDefaultEntrypoint in arrow-adbc's adbc_driver_manager.cc).
func entrypoint(path string) string {
	name := filepath.Base(path)
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimPrefix(name, "lib")
	var b strings.Builder
	for _, tok := range strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' }) {
		r := []rune(tok)
		r[0] = unicode.ToUpper(r[0])
		b.WriteString(string(r))
	}
	e := b.String()
	if !strings.HasPrefix(e, "Adbc") {
		e = "Adbc" + e
	}
	return e + "Init"
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func exports(path string) ([]string, error) {
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		return elfExports(f)
	}
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		return machoExports(f)
	}
	if f, err := pe.Open(path); err == nil {
		defer f.Close()
		return peExports(f)
	}
	return nil, errors.New("not an ELF, Mach-O or PE file")
}

func elfExports(f *elf.File) ([]string, error) {
	syms, err := f.DynamicSymbols()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range syms {
		bind := elf.ST_BIND(s.Info)
		vis := elf.ST_VISIBILITY(s.Other)
		if s.Section == elf.SHN_UNDEF || (bind != elf.STB_GLOBAL && bind != elf.STB_WEAK) ||
			(vis != elf.STV_DEFAULT && vis != elf.STV_PROTECTED) || s.Name == "" {
			continue
		}
		out = append(out, s.Name)
	}
	return out, nil
}

func machoExports(f *macho.File) ([]string, error) {
	if f.Symtab == nil {
		return nil, errors.New("no symbol table")
	}
	const nExt, nPExt, nType, nSect = 0x01, 0x10, 0x0e, 0x0e
	var out []string
	for _, s := range f.Symtab.Syms {
		if s.Type&nExt == 0 || s.Type&nPExt != 0 || s.Type&nType != nSect || s.Sect == 0 {
			continue
		}
		out = append(out, strings.TrimPrefix(s.Name, "_"))
	}
	return out, nil
}

func peExports(f *pe.File) ([]string, error) {
	var dir pe.DataDirectory
	switch oh := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		dir = oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_EXPORT]
	case *pe.OptionalHeader32:
		dir = oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_EXPORT]
	default:
		return nil, errors.New("no optional header")
	}
	if dir.VirtualAddress == 0 {
		return nil, nil
	}
	read := func(rva uint32, n int) ([]byte, error) {
		for _, s := range f.Sections {
			if rva >= s.VirtualAddress && rva < s.VirtualAddress+s.VirtualSize {
				data, err := s.Data()
				if err != nil {
					return nil, err
				}
				off := int(rva - s.VirtualAddress)
				if n < 0 {
					end := off
					for end < len(data) && data[end] != 0 {
						end++
					}
					return data[off:end], nil
				}
				if off+n > len(data) {
					return nil, fmt.Errorf("rva %#x+%d past its section", rva, n)
				}
				return data[off : off+n], nil
			}
		}
		return nil, fmt.Errorf("rva %#x in no section", rva)
	}
	hdr, err := read(dir.VirtualAddress, 40)
	if err != nil {
		return nil, err
	}
	numNames := binary.LittleEndian.Uint32(hdr[24:])
	namesRVA := binary.LittleEndian.Uint32(hdr[32:])
	table, err := read(namesRVA, int(numNames)*4)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, numNames)
	for i := 0; i < int(numNames); i++ {
		name, err := read(binary.LittleEndian.Uint32(table[i*4:]), -1)
		if err != nil {
			return nil, err
		}
		out = append(out, string(name))
	}
	return out, nil
}
