package recover

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"io/ioutil"
	"smirror/config"
	"strings"
	"unsafe"
)

const bufferSize = 1024 * 1024

var lineBreak = []byte{'\n'}

type reader struct {
	count     int32
	recover   *config.Recover
	replacer  *strings.Replacer
	scanner   *bufio.Scanner
	buf       *bytes.Buffer
	transient *bytes.Buffer
	pending   int
	readEOF   bool
	writeEOF  bool
}

func (t *reader) buffer() *bytes.Buffer {
	return t.buf
}

func (t *reader) transform() error {
	if !t.scanner.Scan() {
		t.readEOF = true
	}
	data := t.scanner.Bytes()

	if t.replacer != nil {
		_, _ = t.replacer.WriteString(t.transient, byteToString(data))
		data, _ = ioutil.ReadAll(t.transient)
	}

	if len(data) == 0 {
		return nil
	}
	if t.recover != nil {
		if t.recover.IsJSON() && !json.Valid(data) {
			return nil
		}
		if t.recover.IsCSV() {
			data = t.recoverCSV(data)

		}
	}
	if t.recover.IsJSON() && len(t.recover.Fields) > 0 {
		record := map[string]interface{}{}
		json.Unmarshal(data, record)
		for _, field:= range t.recover.Fields {
			value, ok := record[field.Name]
			if ! ok {
				continue
			}
			record[field.Name] = field.AdjustValue(value)
		}
		if updated, err := json.Marshal(record);err == nil {
			data = updated
		}
	}
	if len(data) == 0 {
		return nil
	}

	if t.count > 0 {
		t.buf.Write(lineBreak)
		t.pending++
	}

	t.pending += len(data)
	t.buf.Write(data)
	t.count++
	return nil
}

func (t *reader) recoverCSV(data []byte) []byte {
	csvReader := t.recover.NewCsvReader(bytes.NewReader(data))
	record, err := csvReader.Read()
	if err != nil {
		return nil
	}


	if len(record) == t.recover.FieldCount {
		t.adjustCSVDataType(record)
		return data
	}
	if len(record) > t.recover.FieldCount {
		record = record[:t.recover.FieldCount]
	} else {
		for len(record) < t.recover.FieldCount {
			record = append(record, "")
		}
	}


	writer := csv.NewWriter(t.transient)
	writer.Comma = csvReader.Comma
	writer.UseCRLF = false
	_ = writer.Write(record)
	writer.Flush()
	data, _ = ioutil.ReadAll(t.transient)
	if len(data) == 0 {
		return data
	}
	return data[:len(data)-1]
}

func (t *reader) Read(p []byte) (n int, err error) {
	if t.writeEOF {
		return 0, io.EOF
	}
	expect := len(p)
	for t.pending < expect && !t.readEOF {
		err := t.transform()
		if err != nil {
			return 0, err
		}
	}

	read, err := t.buffer().Read(p)
	if err == io.EOF || read == 0 {
		if t.readEOF {
			t.writeEOF = true
		} else {
			err = nil
		}
	}
	t.pending -= read
	return read, err
}



func (t *reader) adjustCSVDataType(record []string) {
	if len(t.recover.Fields) == 0 {
		return
	}
	for _, field := range t.recover.Fields {
		if field.Position == nil {
			continue
		}
		index := *field.Position
		if index >= len(record) {
			continue
		}
		record[index] = field.AdjustText(record[index])
	}
}



func NewReader(r io.Reader, rule *config.Rule) (io.Reader, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, bufferSize), 10*bufferSize)
	return &reader{
		recover:   rule.Recover,
		transient: new(bytes.Buffer),
		buf:       new(bytes.Buffer),
		replacer:  rule.NewReplacer(),
		scanner:   scanner,
	}, nil

}

func byteToString(data []byte) string {
	ptr := unsafe.Pointer(&data)
	return *(*string)(ptr)
}
