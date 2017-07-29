package quicklamecsv

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"strings"
)

// From go/src/bytes

type Buffer struct {
	buf       []byte
	off       int
	lastRead  readOp
	bootstrap [1024]byte
}

type readOp int

const (
	opRead      readOp = -1
	opInvalid          = 0
	opReadRune1        = 1
	opReadRune2        = 2
	opReadRune3        = 3
	opReadRune4        = 4
)

var ErrTooLarge = errors.New("bytes.Buffer: too large")

func (b *Buffer) Bytes() []byte { return b.buf[b.off:] }

func (b *Buffer) String() string {
	return string(b.buf[b.off:])
}

func (b *Buffer) Len() int { return len(b.buf) - b.off }

func (b *Buffer) Cap() int { return cap(b.buf) }

func (b *Buffer) Reset() {
	b.buf = b.buf[:0]
	b.off = 0
	b.lastRead = opInvalid
}

func (b *Buffer) tryGrowByReslice(n int) (int, bool) {
	if l := len(b.buf); l+n <= cap(b.buf) {
		b.buf = b.buf[:l+n]
		return l, true
	}
	return 0, false
}

func (b *Buffer) grow(n int) int {
	m := b.Len()
	if m == 0 && b.off != 0 {
		b.Reset()
	}
	if i, ok := b.tryGrowByReslice(n); ok {
		return i
	}
	if b.buf == nil && n <= len(b.bootstrap) {
		b.buf = b.bootstrap[:n]
		return 0
	}
	if m+n <= cap(b.buf)/2 {
		copy(b.buf[:], b.buf[b.off:])
	} else {
		buf := makeSlice(2*cap(b.buf) + n)
		copy(buf, b.buf[b.off:])
		b.buf = buf
	}
	b.off = 0
	b.buf = b.buf[:m+n]
	return m
}

func (b *Buffer) Grow(n int) {
	m := b.grow(n)
	b.buf = b.buf[0:m]
}

func (b *Buffer) Write(p []byte) (n int, err error) {
	b.lastRead = opInvalid
	m, ok := b.tryGrowByReslice(len(p))
	if !ok {
		m = b.grow(len(p))
	}
	return copy(b.buf[m:], p), nil
}

func makeSlice(n int) []byte {
	defer func() {
		if recover() != nil {
			panic(ErrTooLarge)
		}
	}()
	return make([]byte, n)
}

func (b *Buffer) WriteByte(c byte) error {
	b.lastRead = opInvalid
	m, ok := b.tryGrowByReslice(1)
	if !ok {
		m = b.grow(1)
	}
	b.buf[m] = c
	return nil
}

// From the original csv implementation in go/encoding/csv

// A ParseError is returned for parsing errors.
// The first line is 1.  The first column is 0.
type ParseError struct {
	Line   int   // Line where the error occurred
	Column int   // Column (byte index) where the error occurred
	Err    error // The actual error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.Err)
}

// These are the errors that can be returned in ParseError.Error
var (
	ErrTrailingComma = errors.New("extra delimiter at end of line") // no longer used
	ErrBareQuote     = errors.New("bare \" in non-quoted-field")
	ErrQuote         = errors.New("extraneous \" in field")
	ErrFieldCount    = errors.New("wrong number of fields in line")
)

// A Reader reads records from a CSV-encoded file.
//
// As returned by NewReader, a Reader expects input conforming to RFC 4180.
// The exported fields can be changed to customize the details before the
// first call to Read or ReadAll.
//
//
type Reader struct {
	// Comma is the field delimiter.
	// It is set to comma (',') by NewReader.
	Comma byte
	// Comment, if not 0, is the comment character. Lines beginning with the
	// Comment character without preceding whitespace are ignored.
	// With leading whitespace the Comment character becomes part of the
	// field, even if TrimLeadingSpace is true.
	Comment byte
	// FieldsPerRecord is the number of expected fields per record.
	// If FieldsPerRecord is positive, Read requires each record to
	// have the given number of fields. If FieldsPerRecord is 0, Read sets it to
	// the number of fields in the first record, so that future records must
	// have the same field count. If FieldsPerRecord is negative, no check is
	// made and records may have a variable number of fields.
	FieldsPerRecord int
	// If LazyQuotes is true, a quote may appear in an unquoted field and a
	// non-doubled quote may appear in a quoted field.
	LazyQuotes    bool
	TrailingComma bool // ignored; here for backwards compatibility
	// If TrimLeadingSpace is true, leading white space in a field is ignored.
	// This is done even if the field delimiter, Comma, is white space.
	TrimLeadingSpace bool
	// ReuseRecord controls whether calls to Read may return a slice sharing
	// the backing array of the previous call's returned slice for performance.
	// By default, each call to Read returns newly allocated memory owned by the caller.
	ReuseRecord bool

	line   int
	column int
	r      *bufio.Reader
	// lineBuffer holds the unescaped fields read by readField, one after another.
	// The fields can be accessed by using the indexes in fieldIndexes.
	// Example: for the row `a,"b","c""d",e` lineBuffer will contain `abc"de` and
	// fieldIndexes will contain the indexes 0, 1, 2, 5.
	lineBuffer Buffer
	// Indexes of fields inside lineBuffer
	// The i'th field starts at offset fieldIndexes[i] in lineBuffer.
	fieldIndexes []int

	// only used when ReuseRecord == true
	lastRecord []string
}

// NewReader returns a new Reader that reads from r.
func NewReader(r io.Reader) *Reader {
	r2 := Reader{
		Comma: ',',
		r:     bufio.NewReader(r),
	}
	r2.lineBuffer.Grow(1000)
	return &r2
}

// error creates a new ParseError based on err.
func (r *Reader) error(err error) error {
	return &ParseError{
		Line:   r.line,
		Column: r.column,
		Err:    err,
	}
}

// Read reads one record (a slice of fields) from r.
// If the record has an unexpected number of fields,
// Read returns the record along with the error ErrFieldCount.
// Except for that case, Read always returns either a non-nil
// record or a non-nil error, but not both.
// If there is no data left to be read, Read returns nil, io.EOF.
// If ReuseRecord is true, the returned slice may be shared
// between multiple calls to Read.
func (r *Reader) Read() (record []string, err error) {
	if r.ReuseRecord {
		record, err = r.readRecord(r.lastRecord)
		r.lastRecord = record
	} else {
		record, err = r.readRecord(nil)
	}

	return record, err
}

// ReadAll reads all the remaining records from r.
// Each record is a slice of fields.
// A successful call returns err == nil, not err == io.EOF. Because ReadAll is
// defined to read until EOF, it does not treat end of file as an error to be
// reported.
func (r *Reader) ReadAll() (records [][]string, err error) {
	for {
		record, err := r.readRecord(nil)
		if err == io.EOF {
			return records, nil
		}
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
}

// readRecord reads and parses a single csv record from r.
// Unlike parseRecord, readRecord handles FieldsPerRecord.
// If dst has enough capacity it will be used for the returned record.
func (r *Reader) readRecord(dst []string) (record []string, err error) {
	for {
		record, err = r.parseRecord(dst)
		if record != nil {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	if r.FieldsPerRecord > 0 {
		if len(record) != r.FieldsPerRecord {
			r.column = 0 // report at start of record
			return record, r.error(ErrFieldCount)
		}
	} else if r.FieldsPerRecord == 0 {
		r.FieldsPerRecord = len(record)
	}
	return record, nil
}

// readByte reads one byte from r, folding \r\n to \n and keeping track
// of how far into the line we have read.  r.column will point to the start
// of this byte, not the end of this byte.
func (r *Reader) readByte() (byte, error) {
	r1, err := r.r.ReadByte()

	// Handle \r\n here. We make the simplifying assumption that
	// anytime \r is followed by \n that it can be folded to \n.
	// We will not detect files which contain both \r\n and bare \n.
	//fmt.Printf("%q\n", string(r1))
	if r1 == '\r' {
		r1, err = r.r.ReadByte()
		if err == nil {
			if r1 != '\n' {
				r.r.UnreadByte()
				r1 = '\r'
			}
		}
	}
	r.column++
	return r1, err
}

// skip reads bytes up to and including the byte delim or until error.
func (r *Reader) skip(delim byte) error {
	for {
		r1, err := r.readByte()
		if err != nil {
			return err
		}
		if r1 == delim {
			return nil
		}
	}
}

// parseRecord reads and parses a single csv record from r.
// If dst has enough capacity it will be used for the returned fields.
func (r *Reader) parseRecord(dst []string) (fields []string, err error) {
	// Each record starts on a new line. We increment our line
	// number (lines start at 1, not 0) and set column to -1
	// so as we increment in readByte it points to the character we read.
	r.line++
	r.column = -1

	// Peek at the first byte. If it is an error we are done.
	// If we support comments and it is the comment character
	// then skip to the end of line.

	r1, err := r.r.ReadByte()
	if err != nil {
		return nil, err
	}

	if r.Comment != 0 && r1 == r.Comment {
		return nil, r.skip('\n')
	}
	r.r.UnreadByte()

	r.lineBuffer.Reset()
	r.fieldIndexes = r.fieldIndexes[:0]

	// At this point we have at least one field.
	for {
		idx := r.lineBuffer.Len()

		haveField, delim, err := r.parseField()
		if haveField {
			r.fieldIndexes = append(r.fieldIndexes, idx)
		}

		if delim == '\n' || err == io.EOF {
			if len(r.fieldIndexes) == 0 {
				return nil, err
			}
			break
		}

		if err != nil {
			return nil, err
		}
	}

	fieldCount := len(r.fieldIndexes)
	// Using this approach (creating a single string and taking slices of it)
	// means that a single reference to any of the fields will retain the whole
	// string. The risk of a nontrivial space leak caused by this is considered
	// minimal and a tradeoff for better performance through the combined
	// allocations.
	line := r.lineBuffer.String()

	if cap(dst) >= fieldCount {
		fields = dst[:fieldCount]
	} else {
		fields = make([]string, fieldCount)
	}

	for i, idx := range r.fieldIndexes {
		if i == fieldCount-1 {
			fields[i] = line[idx:]
		} else {
			fields[i] = line[idx:r.fieldIndexes[i+1]]
		}
	}

	return fields, nil
}

func isSpace(b byte) bool {
	return b == ' '
}

// parseField parses the next field in the record. The read field is
// appended to r.lineBuffer. Delim is the first character not part of the field
// (r.Comma or '\n').
func (r *Reader) parseField() (haveField bool, delim byte, err error) {
	r1, err := r.readByte()
	for err == nil && r.TrimLeadingSpace && r1 != '\n' && isSpace(r1) {
		r1, err = r.readByte()
	}

	if err == io.EOF && r.column != 0 {
		return true, 0, err
	}
	if err != nil {
		return false, 0, err
	}

	switch r1 {
	case r.Comma:
		// will check below

	case '\n':
		// We are a trailing empty field or a blank line
		if r.column == 0 {
			return false, r1, nil
		}
		return true, r1, nil

	case '"':
		// quoted field
	Quoted:
		for {
			r1, err = r.readByte()
			if err != nil {
				if err == io.EOF {
					if r.LazyQuotes {
						return true, 0, err
					}
					return false, 0, r.error(ErrQuote)
				}
				return false, 0, err
			}
			switch r1 {
			case '"':
				r1, err = r.readByte()
				if err != nil || r1 == r.Comma {
					break Quoted
				}
				if r1 == '\n' {
					return true, r1, nil
				}
				if r1 != '"' {
					if !r.LazyQuotes {
						r.column--
						return false, 0, r.error(ErrQuote)
					}
					// accept the bare quote
					r.lineBuffer.WriteByte('"')
				}
			case '\n':
				r.line++
				r.column = -1
			}
			r.lineBuffer.WriteByte(r1)
		}

	default:
		// unquoted field
		for {
			r.lineBuffer.WriteByte(r1)
			r1, err = r.readByte()
			if err != nil || r1 == r.Comma {
				break
			}
			if r1 == '\n' {
				return true, r1, nil
			}
			if !r.LazyQuotes && r1 == '"' {
				return false, 0, r.error(ErrBareQuote)
			}
		}
	}

	if err != nil {
		if err == io.EOF {
			return true, 0, err
		}
		return false, 0, err
	}

	return true, r1, nil
}

// nTimes is an io.Reader which yields the string s n times.
type mTimes struct {
	s   string
	n   int
	off int
}

func (r *mTimes) Read(p []byte) (n int, err error) {
	for {
		if r.n <= 0 || r.s == "" {
			return n, io.EOF
		}
		n0 := copy(p, r.s[r.off:])
		p = p[n0:]
		n += n0
		r.off += n0
		if r.off == len(r.s) {
			r.off = 0
			r.n--
		}
		if len(p) == 0 {
			return
		}
	}
}

func smash(initReader func(*Reader), rows string) {
	//b.ReportAllocs()
	r := NewReader(&mTimes{s: rows, n: 20})
	//r := NewReader()
	if initReader != nil {
		initReader(r)
	}
	for {
		_, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			//b.Fatal(err)
		}
	}
}

func smash2(r *Reader) {
	for {
		_, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			//b.Fatal(err)
		}
	}
}

func main() {
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	repeat := strings.Repeat(`xxxxxxxxxxxxxxxx,yyyyyyyyyyyyyyyy,zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz,wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww,vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv
xxxxxxxxxxxxxxxxxxxxxxxx,yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy,zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz,wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww,vvvv
,,zzzz,wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww,vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv
xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx,yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy,zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz,wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww,vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv
`, 3)
	for {
		r := NewReader(strings.NewReader(repeat))
		smash2(r)
	}
}
