package transport

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Stdio implements the Transport interface by executing a command
// and communicating with it via stdin/stdout using JSON-RPC.
type Stdio struct {
	process        *stdioProcess
	command        []string
	nextID         int
	debug          bool
	showServerLogs bool
}

// stdioProcess reflects the state of a running command.
type stdioProcess struct {
	stdin            io.WriteCloser
	stdout           io.ReadCloser
	cmd              *exec.Cmd
	stderrBuf        *bytes.Buffer
	isInitializeSent bool
}

// NewStdio creates a new Stdio transport that will execute the given command.
// It communicates with the command using JSON-RPC over stdin/stdout.
func NewStdio(command []string) *Stdio {
	debug := os.Getenv("MCP_DEBUG") == "1"
	return &Stdio{
		command: command,
		nextID:  1,
		debug:   debug,
	}
}

// SetCloseAfterExecute toggles whether the underlying process should be closed
// or kept alive after each call to Execute.
func (t *Stdio) SetCloseAfterExecute(v bool) {
	if v {
		t.process = nil
	} else {
		t.process = &stdioProcess{}
	}
}

// SetShowServerLogs toggles whether to print server logs.
func (t *Stdio) SetShowServerLogs(v bool) {
	t.showServerLogs = v
}

// Execute implements the Transport interface by spawning a subprocess
// and communicating with it via JSON-RPC over stdin/stdout.
func (t *Stdio) Execute(method string, params any) (map[string]any, error) {
	process := t.process
	if process == nil {
		process = &stdioProcess{}
	}

	if process.cmd == nil {
		var err error
		process.stdin, process.stdout, process.cmd, process.stderrBuf, err = t.setupCommand()
		if err != nil {
			return nil, err
		}
	}

	if t.debug {
		fmt.Fprintf(os.Stderr, "DEBUG: Starting initialization\n")
	}

	if !process.isInitializeSent {
		if initErr := t.initialize(process.stdin, process.stdout); initErr != nil {
			t.printStderr(process)
			if t.debug {
				fmt.Fprintf(os.Stderr, "DEBUG: Initialization failed: %v\n", initErr)
			}
			return nil, initErr
		}
		t.printStderr(process)
		process.isInitializeSent = true
	}

	if t.debug {
		fmt.Fprintf(os.Stderr, "DEBUG: Initialization successful, sending method request\n")
	}

	request := Request{
		JSONRPC: "2.0",
		Method:  method,
		ID:      t.nextID,
		Params:  params,
	}
	t.nextID++

	if sendErr := t.sendRequest(process.stdin, request); sendErr != nil {
		return nil, sendErr
	}

	response, err := t.readResponse(process.stdout)
	t.printStderr(process)
	if err != nil {
		return nil, err
	}
	err = t.closeProcess(process)
	if err != nil {
		return nil, err
	}

	return response.Result, nil
}

// printStderr prints and clears any accumulated stderr output.
func (t *Stdio) printStderr(process *stdioProcess) {
	if !t.showServerLogs {
		return
	}
	if process.stderrBuf.Len() > 0 {
		for _, line := range strings.SplitAfter(process.stderrBuf.String(), "\n") {
			line = strings.TrimSuffix(line, "\n")
			if line != "" {
				fmt.Fprintf(os.Stderr, "[>] %s\n", line)
			}
		}
		process.stderrBuf.Reset() // Clear the buffer after reading
	}
}

// closeProcess waits for the command to finish, returning any error.
func (t *Stdio) closeProcess(process *stdioProcess) error {
	if t.process != nil {
		return nil
	}

	_ = process.stdin.Close()

	// Wait for the command to finish with a timeout to prevent zombie processes
	done := make(chan error, 1)
	go func() {
		done <- process.cmd.Wait()
	}()

	select {
	case waitErr := <-done:
		if t.debug {
			fmt.Fprintf(os.Stderr, "DEBUG: Command completed with err: %v\n", waitErr)
		}

		if waitErr != nil && process.stderrBuf.Len() > 0 {
			return fmt.Errorf("command error: %w", waitErr)
		}
	case <-time.After(1 * time.Second):
		if t.debug {
			fmt.Fprintf(os.Stderr, "DEBUG: Command timed out after 1 seconds\n")
		}
		// Kill the process if it times out
		_ = process.cmd.Process.Kill()
	}

	return nil
}

// setupCommand prepares and starts the command, returning the stdin/stdout pipes and any error.
func (t *Stdio) setupCommand() (stdin io.WriteCloser, stdout io.ReadCloser, cmd *exec.Cmd, stderrBuf *bytes.Buffer, err error) {
	if len(t.command) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("no command specified for stdio transport")
	}

	if t.debug {
		fmt.Fprintf(os.Stderr, "DEBUG: Executing command: %v\n", t.command)
	}

	cmd = exec.Command(t.command[0], t.command[1:]...) // #nosec G204

	stdin, err = cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error getting stdin pipe: %w", err)
	}

	stdout, err = cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error getting stdout pipe: %w", err)
	}

	stderrBuf = &bytes.Buffer{}
	cmd.Stderr = stderrBuf

	if err = cmd.Start(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error starting command: %w", err)
	}

	return stdin, stdout, cmd, stderrBuf, nil
}

// initialize sends the initialization request and waits for response and then sends the initialized
// notification.
func (t *Stdio) initialize(stdin io.WriteCloser, stdout io.ReadCloser) error {
	// Create initialization request with current ID
	initRequestID := t.nextID
	initRequest := Request{
		JSONRPC: "2.0",
		Method:  "initialize",
		ID:      initRequestID,
		Params: map[string]any{
			"clientInfo": map[string]any{
				"name":    "f/mcptools",
				"version": "beta",
			},
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{},
		},
	}
	t.nextID++

	if err := t.sendRequest(stdin, initRequest); err != nil {
		return fmt.Errorf("init request failed: %w", err)
	}

	// readResponse now properly checks for matching response ID
	_, err := t.readResponse(stdout)
	if err != nil {
		return fmt.Errorf("init response failed: %w", err)
	}

	// Send initialized notification (notifications don't have IDs)
	initNotification := Request{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}

	if sendErr := t.sendRequest(stdin, initNotification); sendErr != nil {
		return fmt.Errorf("init notification failed: %w", sendErr)
	}

	return nil
}

// sendRequest sends a JSON-RPC request and returns the marshaled request.
func (t *Stdio) sendRequest(stdin io.WriteCloser, request Request) error {
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("error marshaling request: %w", err)
	}
	requestJSON = append(requestJSON, '\n')

	if t.debug {
		fmt.Fprintf(os.Stderr, "DEBUG: Preparing to send request: %s\n", string(requestJSON))
	}

	writer := bufio.NewWriter(stdin)
	n, err := writer.Write(requestJSON)
	if err != nil {
		return fmt.Errorf("error writing bytes to stdin: %w", err)
	}

	if t.debug {
		fmt.Fprintf(os.Stderr, "DEBUG: Wrote %d bytes\n", n)
	}

	if flushErr := writer.Flush(); flushErr != nil {
		return fmt.Errorf("error flushing bytes to stdin: %w", flushErr)
	}

	if t.debug {
		fmt.Fprintf(os.Stderr, "DEBUG: Successfully flushed bytes\n")
	}

	return nil
}

// readResponse reads and parses a JSON-RPC response matching the given request ID.
func (t *Stdio) readResponse(stdout io.ReadCloser) (*Response, error) {
	reader := bufio.NewReader(stdout)

	// Keep track of the expected response ID (the last request ID we sent)
	expectedID := t.nextID - 1

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("error reading from stdout: %w", err)
		}

		if t.debug {
			fmt.Fprintf(os.Stderr, "DEBUG: Read from stdout: %s", string(line))
		}

		if len(line) == 0 {
			return nil, fmt.Errorf("no response from command")
		}

		// First check if this is a notification (no ID field)
		var msg map[string]interface{}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("error unmarshaling message: %w, response: %s", err, string(line))
		}

		// If it's a notification, display it and continue reading
		if methodVal, hasMethod := msg["method"]; hasMethod && msg["id"] == nil {
			method, ok := methodVal.(string)
			if ok && method == "notifications/message" {
				if paramsVal, hasParams := msg["params"].(map[string]interface{}); hasParams {
					level, _ := paramsVal["level"].(string)
					data, _ := paramsVal["data"].(string)

					// Format and print the notification based on level
					switch level {
					case "error":
						fmt.Fprintf(os.Stderr, "\033[31m[ERROR] %s\033[0m\n", data) // Red
					case "warning":
						fmt.Fprintf(os.Stderr, "\033[33m[WARNING] %s\033[0m\n", data) // Yellow
					case "alert":
						fmt.Fprintf(os.Stderr, "\033[35m[ALERT] %s\033[0m\n", data) // Magenta
					case "info":
						fmt.Fprintf(os.Stderr, "\033[36m[INFO] %s\033[0m\n", data) // Cyan
					default:
						fmt.Fprintf(os.Stderr, "\033[37m[%s] %s\033[0m\n", level, data) // White for unknown levels
					}
				}
			} else {
				// For other notification types
				fmt.Fprintf(os.Stderr, "[Notification] %s\n", string(line))
			}
			continue
		}

		// Parse as a proper response
		var response Response
		if unmarshalErr := json.Unmarshal(line, &response); unmarshalErr != nil {
			return nil, fmt.Errorf("error unmarshaling response: %w, response: %s", unmarshalErr, string(line))
		}

		// If this response has an ID field and it matches our expected ID, or if it has an error, return it
		if response.ID == expectedID || response.Error != nil {
			if response.Error != nil {
				return nil, fmt.Errorf("RPC error %d: %s", response.Error.Code, response.Error.Message)
			}

			if t.debug {
				fmt.Fprintf(os.Stderr, "DEBUG: Successfully parsed response with matching ID: %d\n", response.ID)
			}

			return &response, nil
		}

		// Otherwise, this is a response for a different request
		if t.debug {
			fmt.Fprintf(os.Stderr, "DEBUG: Received response for request ID %d, expecting %d. Continuing to read.\n",
				response.ID, expectedID)
		}
	}
}
