package spgz

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

const (
	headerMagic = "SPGZ0001"
	headerSize  = 4096
)

const (
	defBlockSize = 1*1024*1024 - 1
)

const (
	blkUncompressed byte = iota
	blkCompressed
)

var (
	ErrInvalidFormat         = errors.New("Invalid file format")
	ErrPunchHoleNotSupported = errors.New("The filesystem does not support punching holes. Use xfs or ext4")
	ErrFileIsDirectory       = errors.New("File cannot be a directory")
)

type block struct {
	f                   *compFile
	num                 int64
	data                []byte
	rawBlock, dataBlock []byte
	blockIsRaw          bool
	dirty               bool
}

type compFile struct {
	f         SparseFile
	blockSize int64
	block     block
	loaded    bool

	offset int64
}

func (b *block) init(f *compFile) {
	b.f = f
	b.dirty = false
	b.blockIsRaw = false
}

func (b *block) load(num int64) error {
	// log.Printf("Loading block %d", num)
	b.num = num
	if b.rawBlock == nil {
		b.rawBlock = make([]byte, b.f.blockSize+1)
	} else {
		b.rawBlock = b.rawBlock[:b.f.blockSize+1]
	}

	if b.dataBlock == nil {
		b.dataBlock = make([]byte, b.f.blockSize)
	}

	n, err := b.f.f.ReadAt(b.rawBlock, headerSize+num*(b.f.blockSize+1))
	if err != nil {
		if err == io.EOF {
			if n > 0 {
				b.rawBlock = b.rawBlock[:n]
				err = nil
			} else {
				b.data = b.dataBlock[:0]
				b.blockIsRaw = false
				return err
			}
		} else {
			b.data = b.dataBlock[:0]
			b.blockIsRaw = false
			return err
		}
	}

	switch b.rawBlock[0] {
	case blkUncompressed:
		b.data = b.rawBlock[1:]
		b.blockIsRaw = true
	case blkCompressed:
		err = b.loadCompressed()
	}
	b.dirty = false
	// log.Printf("Loaded, size %d\n", len(b.data))
	return err

}

func (b *block) loadCompressed() error {
	// log.Println("Block is compressed")
	z, err := gzip.NewReader(bytes.NewBuffer(b.rawBlock[1:]))
	if err != nil {
		return err
	}
	z.Multistream(false)

	buf := bytes.NewBuffer(b.dataBlock[:0])

	_, err = io.Copy(buf, z)
	if err != nil {
		return err
	}
	b.data = buf.Bytes()
	b.blockIsRaw = false

	l := int64(len(b.data))
	if l < b.f.blockSize {
		o, err := b.f.f.Seek(0, os.SEEK_END)
		if err != nil {
			return err
		}
		lastBlockNum := (o - headerSize) / (b.f.blockSize + 1)
		if lastBlockNum > b.num {
			b.data = b.data[:b.f.blockSize]
			for i := l; i < b.f.blockSize; i++ {
				b.data[i] = 0
			}
		}
	}

	return nil
}

func (b *block) store(truncate bool) (err error) {
	// log.Printf("Storing block %d", b.num)

	var curOffset int64

	if IsBlockZero(b.data) {
		// log.Println("Block is all zeroes")
		err = b.f.f.PunchHole(headerSize+b.num*(b.f.blockSize+1), int64(len(b.data))+1)
		if err != nil {
			err = ErrPunchHoleNotSupported
			return err
		}
		var o int64
		o, err = b.f.f.Seek(0, os.SEEK_END)
		if err != nil {
			return err
		}
		curOffset = headerSize + b.num*(b.f.blockSize+1) + int64(len(b.data)) + 1
		if o < curOffset {
			err = b.f.f.Truncate(curOffset) // Extend the file
			if err != nil {
				return err
			}
		}
	} else {
		b.prepareWrite()

		buf := bytes.NewBuffer(b.rawBlock[:0])

		reader := bytes.NewBuffer(b.data)

		buf.WriteByte(blkCompressed)

		w := gzip.NewWriter(buf)
		_, err = io.Copy(w, reader)
		if err != nil {
			return err
		}
		err = w.Close()
		if err != nil {
			return err
		}
		bb := buf.Bytes()
		n := len(bb)
		if n+1 < len(b.data)-2*4096 { // save at least 2 blocks
			// log.Printf("Storing compressed, size %d\n", n - 1)
			_, err = b.f.f.WriteAt(bb, headerSize+b.num*(b.f.blockSize+1))
			if err != nil {
				return err
			}

			curOffset = headerSize + b.num*(b.f.blockSize+1) + int64(n)
			err = b.f.f.PunchHole(curOffset, b.f.blockSize-int64(n))
			if err != nil {
				err = ErrPunchHoleNotSupported
			}

		} else {
			// log.Println("Storing uncompressed")
			buf.Reset()
			buf.WriteByte(blkUncompressed)
			buf.Write(b.data)
			_, err = b.f.f.WriteAt(buf.Bytes(), headerSize+b.num*(b.f.blockSize+1))
			curOffset = headerSize + b.num*(b.f.blockSize+1) + int64(len(b.data)) + 1
		}
	}

	if err != nil {
		return err
	}

	b.dirty = false

	var o int64
	o, err = b.f.f.Seek(0, os.SEEK_END)
	if err != nil {
		return err
	}

	// log.Printf("curOffset: %d, size: %d\n", curOffset, o)

	if truncate || o < headerSize+(b.num+1)*(b.f.blockSize+1) {
		if o > curOffset {
			err = b.f.f.Truncate(curOffset)
		}
	}
	return
}

func (b *block) prepareWrite() {
	if b.blockIsRaw {
		if b.dataBlock == nil {
			b.dataBlock = make([]byte, len(b.data), b.f.blockSize)
		} else {
			b.dataBlock = b.dataBlock[:len(b.data)]
		}
		copy(b.dataBlock, b.data)
		b.data = b.dataBlock
		b.blockIsRaw = false
	}
}

func (f *compFile) Read(buf []byte) (n int, err error) {
	// log.Printf("Read %d bytes at %d\n", len(buf), f.offset)
	err = f.load()
	if err != nil {
		return 0, err
	}
	o := f.offset - f.block.num*f.blockSize
	n = copy(buf, f.block.data[o:])
	f.offset += int64(n)
	if n == 0 {
		err = io.EOF
	}
	return
}

func (f *compFile) load() error {
	num := f.offset / f.blockSize
	if num != f.block.num || !f.loaded {
		if f.block.dirty {
			err := f.block.store(false)
			if err != nil {
				return err
			}
		}
		err := f.block.load(num)
		f.loaded = true
		return err
	}
	return nil
}

func (f *compFile) Write(buf []byte) (n int, err error) {
	for len(buf) > 0 {
		// log.Printf("Writing %d bytes\n", len(buf))
		err = f.load()
		if err != nil {
			if err != io.EOF {
				return 0, err
			}
			err = nil
		}

		o := f.offset - f.block.num*f.blockSize
		newBlockSize := o + int64(len(buf))
		if newBlockSize > f.blockSize {
			newBlockSize = f.blockSize
		}
		l := int64(len(f.block.data))
		if newBlockSize > l {
			f.block.data = f.block.data[:newBlockSize]
			for i := l; i < o; i++ {
				f.block.data[i] = 0
			}
		}
		nn := copy(f.block.data[o:], buf)
		f.block.dirty = true
		n += nn
		f.offset += int64(nn)
		buf = buf[nn:]
	}
	return
}

func (f *compFile) Size() (int64, error) {
	o, err := f.f.Seek(0, os.SEEK_END)
	if err != nil {
		return 0, err
	}
	if o <= headerSize {
		return 0, nil
	}
	lastBlockNum := (o - headerSize) / (f.blockSize + 1)
	if f.loaded && lastBlockNum <= f.block.num {
		// Last block is currently loaded
		return f.block.num*f.blockSize + int64(len(f.block.data)), nil
	}

	b := &block{
		f: f,
	}

	err = b.load(lastBlockNum)

	if err != nil {
		return 0, err
	}
	return lastBlockNum*f.blockSize + int64(len(b.data)), nil
}

func (f *compFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case os.SEEK_SET:
		f.offset = offset
		return f.offset, nil
	case os.SEEK_CUR:
		f.offset += offset
		return f.offset, nil
	case os.SEEK_END:
		size, err := f.Size()
		if err != nil {
			return f.offset, err
		}
		f.offset = size + offset
		return f.offset, nil
	}
	return f.offset, os.ErrInvalid
}

func (f *compFile) Truncate(size int64) error {
	blockNum := size / f.blockSize
	b := &block{
		f: f,
	}

	err := b.load(blockNum)
	if err != nil {
		if err == io.EOF {
			err = nil
		} else {
			return err
		}
	}

	newLen := int(size - blockNum*f.blockSize)

	if len(b.data) != newLen {
		b.data = b.data[:newLen]
		err = b.store(true)
	}

	return err
}

func (f *compFile) WriteTo(w io.Writer) (n int64, err error) {
	for {
		err = f.load()
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return
		}
		buf := f.block.data[f.offset-f.block.num*f.blockSize:]
		if len(buf) == 0 {
			return
		}
		var written int
		written, err = w.Write(buf)
		f.offset += int64(written)
		n += int64(written)
		if err != nil {
			return
		}
	}
}

func (f *compFile) ReadFrom(rd io.Reader) (n int64, err error) {
	for {
		err = f.load()
		if err != nil {
			if err != io.EOF {
				return
			}
		}
		o := int(f.offset - f.block.num*f.blockSize)
		buf := f.block.data[o:f.blockSize]
		var r int
		r, err = rd.Read(buf)
		nl := o + r
		if nl > len(f.block.data) {
			f.block.data = f.block.data[:nl]
		}
		f.offset += int64(r)
		n += int64(r)
		f.block.dirty = true
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return
		}
	}
}

func (f *compFile) Sync() error {
	if f.block.dirty {
		err := f.block.store(false)
		if err != nil {
			return err
		}
	}
	return f.f.Sync()
}

func (f *compFile) Close() error {
	if f.block.dirty {
		err := f.block.store(false)
		if err != nil {
			return err
		}
	}
	return f.f.Close()
}

func (f *compFile) init(flag int) error {
	f.block.init(f)

	// Trying to read the header
	buf := make([]byte, len(headerMagic)+4)

	_, err := io.ReadFull(f.f, buf)
	if err != nil {
		if err == io.EOF {
			// Empty file
			if flag&os.O_WRONLY != 0 || flag&os.O_RDWR != 0 {
				w := bytes.NewBuffer(buf[:0])
				w.WriteString(headerMagic)
				binary.Write(w, binary.LittleEndian, uint32((defBlockSize+1)/4096))
				_, err = f.f.Write(w.Bytes())
				if err != nil {
					return err
				}
				f.blockSize = defBlockSize
				return nil
			}
		}
		if err == io.ErrUnexpectedEOF {
			return ErrInvalidFormat
		}
		return err
	}
	if string(buf[:8]) != headerMagic {
		return ErrInvalidFormat
	}
	w := bytes.NewBuffer(buf[8:])
	var bs uint32
	binary.Read(w, binary.LittleEndian, &bs)
	f.blockSize = int64(bs*4096) - 1
	return nil
}

func OpenFile(name string, flag int, perm os.FileMode) (f *compFile, err error) {
	var ff *os.File
	ff, err = os.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}

	f = &compFile{
		f: NewSparseFile(ff),
	}

	err = f.init(flag)
	if err != nil {
		f.f.Close()
		return nil, err
	}

	return f, nil
}

func NewFromFile(file *os.File, flag int) (f *compFile, err error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, ErrFileIsDirectory
	}

	f = &compFile{
		f: NewSparseFile(file),
	}

	err = f.init(flag)
	if err != nil {
		return nil, err
	}

	return f, nil
}

func NewFromSparseFile(file SparseFile, flag int) (f *compFile, err error) {
	f = &compFile{
		f: file,
	}

	err = f.init(flag)
	if err != nil {
		return nil, err
	}

	return f, nil
}
