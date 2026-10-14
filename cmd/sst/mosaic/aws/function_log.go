package aws

import "io"

// writeInvocationLogs writes one file per invocation: its input, every log
// line and its response or error. The file is closed when the invocation ends.
func writeInvocationLogs(evts <-chan interface{}, open func(functionID string, requestID string) (io.WriteCloser, error)) {
	logs := map[string]io.WriteCloser{}
	write := func(functionID string, requestID string, text string) {
		log, ok := logs[requestID]
		if !ok {
			var err error
			log, err = open(functionID, requestID)
			if err != nil {
				return
			}
			logs[requestID] = log
		}
		io.WriteString(log, text)
	}
	finish := func(requestID string) {
		if log, ok := logs[requestID]; ok {
			log.Close()
			delete(logs, requestID)
		}
	}

	for evt := range evts {
		switch evt := evt.(type) {
		case *FunctionInvokedEvent:
			write(evt.FunctionID, evt.RequestID, "invocation "+evt.RequestID+"\n"+string(evt.Input)+"\n")
		case *FunctionLogEvent:
			write(evt.FunctionID, evt.RequestID, evt.Line+"\n")
		case *FunctionResponseEvent:
			write(evt.FunctionID, evt.RequestID, "response "+evt.RequestID+"\n"+string(evt.Output)+"\n")
			finish(evt.RequestID)
		case *FunctionErrorEvent:
			write(evt.FunctionID, evt.RequestID, evt.ErrorType+": "+evt.ErrorMessage+"\n")
			finish(evt.RequestID)
		}
	}
}
