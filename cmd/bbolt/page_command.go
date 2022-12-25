package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unsafe"
)

// PageCommand represents the "page" command execution.
type PageCommand struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// newPageCommand returns a PageCommand.
func newPageCommand(m *Main) *PageCommand {
	return &PageCommand{
		Stdin:  m.Stdin,
		Stdout: m.Stdout,
		Stderr: m.Stderr,
	}
}

// Run executes the command.
func (cmd *PageCommand) Run(args ...string) error {
	// Parse flags.
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	help := fs.Bool("h", false, "")
	all := fs.Bool("all", false, "list all pages")
	formatValue := fs.String("format-value", "auto", "One of: "+FORMAT_MODES+" . Applies to values on the leaf page.")

	if err := fs.Parse(args); err != nil {
		return err
	} else if *help {
		fmt.Fprintln(cmd.Stderr, cmd.Usage())
		return ErrUsage
	}

	// Require database path and page id.
	path := fs.Arg(0)
	if path == "" {
		return ErrPathRequired
	} else if _, err := os.Stat(path); os.IsNotExist(err) {
		return ErrFileNotFound
	}

	if !*all {
		// Read page ids.
		pageIDs, err := stringToPages(fs.Args()[1:])
		if err != nil {
			return err
		} else if len(pageIDs) == 0 {
			return ErrPageIDRequired
		}
		cmd.printPages(pageIDs, path, formatValue)
	} else {
		cmd.printAllPages(path, formatValue)
	}
	return nil
}

func (cmd *PageCommand) printPages(pageIDs []uint64, path string, formatValue *string) {
	// Print each page listed.
	for i, pageID := range pageIDs {
		// Print a separator.
		if i > 0 {
			fmt.Fprintln(cmd.Stdout, "===============================================")
		}
		_, err2 := cmd.printPage(path, pageID, *formatValue)
		if err2 != nil {
			fmt.Fprintf(cmd.Stdout, "Prining page %d failed: %s. Continuuing...\n", pageID, err2)
		}
	}
}

func (cmd *PageCommand) printAllPages(path string, formatValue *string) {
	_, hwm, err := ReadPageAndHWMSize(path)
	if err != nil {
		fmt.Fprintf(cmd.Stdout, "cannot read number of pages: %v", err)
	}

	// Print each page listed.
	for pageID := uint64(0); pageID < uint64(hwm); {
		// Print a separator.
		if pageID > 0 {
			fmt.Fprintln(cmd.Stdout, "===============================================")
		}
		overflow, err2 := cmd.printPage(path, pageID, *formatValue)
		if err2 != nil {
			fmt.Fprintf(cmd.Stdout, "Prining page %d failed: %s. Continuuing...\n", pageID, err2)
			pageID++
		} else {
			pageID += uint64(overflow) + 1
		}
	}
}

// printPage prints given page to cmd.Stdout and returns error or number of interpreted pages.
func (cmd *PageCommand) printPage(path string, pageID uint64, formatValue string) (numPages uint32, reterr error) {
	defer func() {
		if err := recover(); err != nil {
			reterr = fmt.Errorf("%s", err)
		}
	}()

	// Retrieve page info and page size.
	p, buf, err := ReadPage(path, pageID)
	if err != nil {
		return 0, err
	}

	// Print basic page info.
	fmt.Fprintf(cmd.Stdout, "Page ID:    %d\n", p.id)
	fmt.Fprintf(cmd.Stdout, "Page Type:  %s\n", p.Type())
	fmt.Fprintf(cmd.Stdout, "Total Size: %d bytes\n", len(buf))
	fmt.Fprintf(cmd.Stdout, "Overflow pages: %d\n", p.overflow)

	// Print type-specific data.
	switch p.Type() {
	case "meta":
		err = cmd.PrintMeta(cmd.Stdout, buf)
	case "leaf":
		err = cmd.PrintLeaf(cmd.Stdout, buf, formatValue)
	case "branch":
		err = cmd.PrintBranch(cmd.Stdout, buf)
	case "freelist":
		err = cmd.PrintFreelist(cmd.Stdout, buf)
	}
	if err != nil {
		return 0, err
	}
	return p.overflow, nil
}

// PrintMeta prints the data from the meta page.
func (cmd *PageCommand) PrintMeta(w io.Writer, buf []byte) error {
	m := (*meta)(unsafe.Pointer(&buf[PageHeaderSize]))
	fmt.Fprintf(w, "Version:    %d\n", m.version)
	fmt.Fprintf(w, "Page Size:  %d bytes\n", m.pageSize)
	fmt.Fprintf(w, "Flags:      %08x\n", m.flags)
	fmt.Fprintf(w, "Root:       <pgid=%d>\n", m.root.root)
	fmt.Fprintf(w, "Freelist:   <pgid=%d>\n", m.freelist)
	fmt.Fprintf(w, "HWM:        <pgid=%d>\n", m.pgid)
	fmt.Fprintf(w, "Txn ID:     %d\n", m.txid)
	fmt.Fprintf(w, "Checksum:   %016x\n", m.checksum)
	fmt.Fprintf(w, "\n")
	return nil
}

// PrintLeaf prints the data for a leaf page.
func (cmd *PageCommand) PrintLeaf(w io.Writer, buf []byte, formatValue string) error {
	p := (*page)(unsafe.Pointer(&buf[0]))

	// Print number of items.
	fmt.Fprintf(w, "Item Count: %d\n", p.count)
	fmt.Fprintf(w, "\n")

	// Print each key/value.
	for i := uint16(0); i < p.count; i++ {
		e := p.leafPageElement(i)

		// Format key as string.
		var k string
		if isPrintable(string(e.key())) {
			k = fmt.Sprintf("%q", string(e.key()))
		} else {
			k = fmt.Sprintf("%x", string(e.key()))
		}

		// Format value as string.
		var v string
		if (e.flags & uint32(bucketLeafFlag)) != 0 {
			b := (*bucket)(unsafe.Pointer(&e.value()[0]))
			v = fmt.Sprintf("<pgid=%d,seq=%d>", b.root, b.sequence)
		} else {
			var err error
			v, err = formatBytes(e.value(), formatValue)
			if err != nil {
				return err
			}
		}

		fmt.Fprintf(w, "%s: %s\n", k, v)
	}
	fmt.Fprintf(w, "\n")
	return nil
}

// PrintBranch prints the data for a leaf page.
func (cmd *PageCommand) PrintBranch(w io.Writer, buf []byte) error {
	p := (*page)(unsafe.Pointer(&buf[0]))

	// Print number of items.
	fmt.Fprintf(w, "Item Count: %d\n", p.count)
	fmt.Fprintf(w, "\n")

	// Print each key/value.
	for i := uint16(0); i < p.count; i++ {
		e := p.branchPageElement(i)

		// Format key as string.
		var k string
		if isPrintable(string(e.key())) {
			k = fmt.Sprintf("%q", string(e.key()))
		} else {
			k = fmt.Sprintf("%x", string(e.key()))
		}

		fmt.Fprintf(w, "%s: <pgid=%d>\n", k, e.pgid)
	}
	fmt.Fprintf(w, "\n")
	return nil
}

// PrintFreelist prints the data for a freelist page.
func (cmd *PageCommand) PrintFreelist(w io.Writer, buf []byte) error {
	p := (*page)(unsafe.Pointer(&buf[0]))

	// Check for overflow and, if present, adjust starting index and actual element count.
	idx, count := 0, int(p.count)
	if p.count == 0xFFFF {
		idx = 1
		count = int(((*[maxAllocSize]pgid)(unsafe.Pointer(&p.ptr)))[0])
	}

	// Print number of items.
	fmt.Fprintf(w, "Item Count: %d\n", count)
	fmt.Fprintf(w, "Overflow: %d\n", p.overflow)

	fmt.Fprintf(w, "\n")

	// Print each page in the freelist.
	ids := (*[maxAllocSize]pgid)(unsafe.Pointer(&p.ptr))
	for i := idx; i < count; i++ {
		fmt.Fprintf(w, "%d\n", ids[i])
	}
	fmt.Fprintf(w, "\n")
	return nil
}

// PrintPage prints a given page as hexadecimal.
func (cmd *PageCommand) PrintPage(w io.Writer, r io.ReaderAt, pageID int, pageSize int) error {
	const bytesPerLineN = 16

	// Read page into buffer.
	buf := make([]byte, pageSize)
	addr := pageID * pageSize
	if n, err := r.ReadAt(buf, int64(addr)); err != nil {
		return err
	} else if n != pageSize {
		return io.ErrUnexpectedEOF
	}

	// Write out to writer in 16-byte lines.
	var prev []byte
	var skipped bool
	for offset := 0; offset < pageSize; offset += bytesPerLineN {
		// Retrieve current 16-byte line.
		line := buf[offset : offset+bytesPerLineN]
		isLastLine := (offset == (pageSize - bytesPerLineN))

		// If it's the same as the previous line then print a skip.
		if bytes.Equal(line, prev) && !isLastLine {
			if !skipped {
				fmt.Fprintf(w, "%07x *\n", addr+offset)
				skipped = true
			}
		} else {
			// Print line as hexadecimal in 2-byte groups.
			fmt.Fprintf(w, "%07x %04x %04x %04x %04x %04x %04x %04x %04x\n", addr+offset,
				line[0:2], line[2:4], line[4:6], line[6:8],
				line[8:10], line[10:12], line[12:14], line[14:16],
			)

			skipped = false
		}

		// Save the previous line.
		prev = line
	}
	fmt.Fprint(w, "\n")

	return nil
}

// Usage returns the help message.
func (cmd *PageCommand) Usage() string {
	return strings.TrimLeft(`
usage: bolt page PATH pageid [pageid...]
   or: bolt page --all PATH

Additional options include:

	--all
		prints all pages (only skips pages that were considered successful overflow pages) 
	--format-value=`+FORMAT_MODES+` (default: auto)
		prints values (on the leaf page) using the given format.

Page prints one or more pages in human readable format.
`, "\n")
}
