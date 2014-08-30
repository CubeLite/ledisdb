package ledis

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"github.com/siddontang/go-log/log"
	"github.com/siddontang/ledisdb/store/driver"
	"io"
	"os"
)

var (
	ErrSkipEvent = errors.New("skip to next event")
)

var (
	errInvalidBinLogEvent = errors.New("invalid binglog event")
	errInvalidBinLogFile  = errors.New("invalid binlog file")
)

type replBatch struct {
	wb         driver.IWriteBatch
	events     [][]byte
	createTime uint32
	l          *Ledis
}

func (b *replBatch) Commit() error {
	b.l.commitLock.Lock()
	defer b.l.commitLock.Unlock()

	err := b.wb.Commit()
	if err != nil {
		b.Rollback()
		return err
	}

	if b.l.binlog != nil {
		if err = b.l.binlog.Log(b.events...); err != nil {
			b.Rollback()
			return err
		}
	}

	return nil
}

func (b *replBatch) Rollback() error {
	b.wb.Rollback()
	b.events = [][]byte{}
	b.createTime = 0
	return nil
}

func (l *Ledis) replicateEvent(b *replBatch, event []byte) error {
	if len(event) == 0 {
		return errInvalidBinLogEvent
	}

	logType := uint8(event[0])
	switch logType {
	case BinLogTypePut:
		return l.replicatePutEvent(b, event)
	case BinLogTypeDeletion:
		return l.replicateDeleteEvent(b, event)
	default:
		return errInvalidBinLogEvent
	}
}

func (l *Ledis) replicatePutEvent(b *replBatch, event []byte) error {
	key, value, err := decodeBinLogPut(event)
	if err != nil {
		return err
	}

	b.wb.Put(key, value)

	if b.l.binlog != nil {
		b.events = append(b.events, event)
	}

	return nil
}

func (l *Ledis) replicateDeleteEvent(b *replBatch, event []byte) error {
	key, err := decodeBinLogDelete(event)
	if err != nil {
		return err
	}

	b.wb.Delete(key)

	if b.l.binlog != nil {
		b.events = append(b.events, event)
	}

	return nil
}

func ReadEventFromReader(rb io.Reader, f func(createTime uint32, event []byte) error) error {
	var createTime uint32
	var dataLen uint32
	var dataBuf bytes.Buffer
	var err error

	for {
		if err = binary.Read(rb, binary.BigEndian, &createTime); err != nil {
			if err == io.EOF {
				break
			} else {
				return err
			}
		}

		if err = binary.Read(rb, binary.BigEndian, &dataLen); err != nil {
			return err
		}

		if _, err = io.CopyN(&dataBuf, rb, int64(dataLen)); err != nil {
			return err
		}

		err = f(createTime, dataBuf.Bytes())
		if err != nil && err != ErrSkipEvent {
			return err
		}

		dataBuf.Reset()
	}

	return nil
}

func (l *Ledis) ReplicateFromReader(rb io.Reader) error {
	b := new(replBatch)

	b.wb = l.ldb.NewWriteBatch()
	b.l = l

	f := func(createTime uint32, event []byte) error {
		if b.createTime == 0 {
			b.createTime = createTime
		} else if b.createTime != createTime {
			if err := b.Commit(); err != nil {
				log.Fatal("replication error %s, skip to next", err.Error())
				return ErrSkipEvent
			}
			b.createTime = createTime
		}

		err := l.replicateEvent(b, event)
		if err != nil {
			log.Fatal("replication error %s, skip to next", err.Error())
			return ErrSkipEvent
		}
		return nil
	}

	err := ReadEventFromReader(rb, f)
	if err != nil {
		b.Rollback()
		return err
	}
	return b.Commit()
}

func (l *Ledis) ReplicateFromData(data []byte) error {
	rb := bytes.NewReader(data)

	err := l.ReplicateFromReader(rb)

	return err
}

func (l *Ledis) ReplicateFromBinLog(filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}

	rb := bufio.NewReaderSize(f, 4096)

	err = l.ReplicateFromReader(rb)

	f.Close()

	return err
}

func (l *Ledis) ReadEventsTo(info *MasterInfo, w io.Writer) (n int, err error) {
	n = 0
	if l.binlog == nil {
		//binlog not supported
		info.LogFileIndex = 0
		info.LogPos = 0
		return
	}

	index := info.LogFileIndex
	offset := info.LogPos

	filePath := l.binlog.FormatLogFilePath(index)

	var f *os.File
	f, err = os.Open(filePath)
	if os.IsNotExist(err) {
		lastIndex := l.binlog.LogFileIndex()

		if index == lastIndex {
			//no binlog at all
			info.LogPos = 0
		} else {
			//slave binlog info had lost
			info.LogFileIndex = -1
		}
	}

	if err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return
	}

	defer f.Close()

	var fileSize int64
	st, _ := f.Stat()
	fileSize = st.Size()

	if fileSize == info.LogPos {
		return
	}

	if _, err = f.Seek(offset, os.SEEK_SET); err != nil {
		//may be invliad seek offset
		return
	}

	var lastCreateTime uint32 = 0
	var createTime uint32
	var dataLen uint32

	for {
		if err = binary.Read(f, binary.BigEndian, &createTime); err != nil {
			if err == io.EOF {
				//we will try to use next binlog
				if index < l.binlog.LogFileIndex() {
					info.LogFileIndex += 1
					info.LogPos = 0
				}
				err = nil
				return
			} else {
				return
			}
		}

		if lastCreateTime == 0 {
			lastCreateTime = createTime
		} else if lastCreateTime != createTime {
			return
		}

		if err = binary.Read(f, binary.BigEndian, &dataLen); err != nil {
			return
		}

		if err = binary.Write(w, binary.BigEndian, createTime); err != nil {
			return
		}

		if err = binary.Write(w, binary.BigEndian, dataLen); err != nil {
			return
		}

		if _, err = io.CopyN(w, f, int64(dataLen)); err != nil {
			return
		}

		n += (8 + int(dataLen))
		info.LogPos = info.LogPos + 8 + int64(dataLen)
	}

	return
}
