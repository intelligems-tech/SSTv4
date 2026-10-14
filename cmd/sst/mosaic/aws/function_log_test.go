package aws

import (
	"bytes"
	"io"
	"testing"
)

type memLog struct {
	bytes.Buffer
	closed bool
}

func (m *memLog) Close() error { m.closed = true; return nil }

func TestWriteInvocationLogsKeepsTheFirstEventAndClosesOnCompletion(t *testing.T) {
	evts := make(chan interface{}, 10)
	evts <- &FunctionInvokedEvent{FunctionID: "fn", RequestID: "r1", Input: []byte(`{"a":1}`)}
	evts <- &FunctionLogEvent{FunctionID: "fn", RequestID: "r1", Line: "hello"}
	evts <- &FunctionResponseEvent{FunctionID: "fn", RequestID: "r1", Output: []byte(`"ok"`)}
	evts <- &FunctionInvokedEvent{FunctionID: "fn", RequestID: "r2"}
	evts <- &FunctionErrorEvent{FunctionID: "fn", RequestID: "r2", ErrorType: "Error", ErrorMessage: "boom"}
	close(evts)

	files := map[string]*memLog{}
	writeInvocationLogs(evts, func(functionID string, requestID string) (io.WriteCloser, error) {
		files[requestID] = &memLog{}
		return files[requestID], nil
	})

	want := "invocation r1\n{\"a\":1}\nhello\nresponse r1\n\"ok\"\n"
	if got := files["r1"].String(); got != want {
		t.Fatalf("r1 log = %q, want %q", got, want)
	}
	if got := files["r2"].String(); got != "invocation r2\n\nError: boom\n" {
		t.Fatalf("r2 log = %q", got)
	}
	if !files["r1"].closed || !files["r2"].closed {
		t.Fatal("finished invocations must close their log files")
	}
}
