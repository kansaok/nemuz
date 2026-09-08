package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultCallTimeout bounds a single tool call. A plugin that has not answered
// in this long is treated as wedged.
const DefaultCallTimeout = 2 * time.Minute

// DefaultStartTimeout bounds the handshake. Startup should be fast; a plugin
// that is slow here is usually misconfigured rather than busy.
const DefaultStartTimeout = 15 * time.Second

// maxLine caps one JSON-RPC message. A plugin returning more than this is
// returning a file, not a tool result, and should have written it to the
// workspace instead.
const maxLine = 8 << 20 // 8 MiB

// stderrKeep is how many trailing stderr lines are retained for diagnostics.
// A crashed plugin's last words are usually the useful ones.
const stderrKeep = 20

// Options configures a plugin process.
type Options struct {
	// Command is the argv to execute. The first element is the program.
	Command []string
	// Dir is the working directory for the process.
	Dir string
	// Workspace is passed to the plugin at initialize.
	Workspace string
	// Env replaces the environment entirely when non-nil.
	//
	// The default is an empty environment, not the host's. A plugin that
	// needs a variable must be given it deliberately, so a stray API key in
	// the host's environment is not handed to every plugin that runs.
	Env []string
	// HostVersion is reported to the plugin at initialize.
	HostVersion string
	// StartTimeout overrides DefaultStartTimeout.
	StartTimeout time.Duration
	// CallTimeout overrides DefaultCallTimeout.
	CallTimeout time.Duration
}

// Client is a running plugin process.
//
// Requests are serialised: one is in flight at a time. Tool calls within a turn
// are sequential anyway, and serialising removes a whole class of correlation
// bugs for no practical cost.
type Client struct {
	opts     Options
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	manifest Manifest

	mu     sync.Mutex
	nextID int64

	responses chan Response
	readErr   chan error
	exited    chan struct{}

	stderrMu sync.Mutex
	stderr   []string

	closeOnce sync.Once
	closeErr  error
}

// Start launches a plugin and completes the handshake.
func Start(ctx context.Context, opts Options) (*Client, error) {
	if len(opts.Command) == 0 {
		return nil, errors.New("plugin: no command given")
	}

	cmd := exec.Command(opts.Command[0], opts.Command[1:]...)
	cmd.Dir = opts.Dir
	if opts.Env != nil {
		cmd.Env = opts.Env
	} else {
		cmd.Env = []string{}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin: start %s: %w", opts.Command[0], err)
	}

	c := &Client{
		opts:      opts,
		cmd:       cmd,
		stdin:     stdin,
		responses: make(chan Response, 1),
		readErr:   make(chan error, 1),
		exited:    make(chan struct{}),
	}
	go c.readLoop(stdout)
	go c.drainStderr(stderr)
	go func() {
		_ = cmd.Wait()
		close(c.exited)
	}()

	if err := c.handshake(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Manifest returns what the plugin declared at startup.
func (c *Client) Manifest() Manifest { return c.manifest }

// Name returns the plugin's declared name.
func (c *Client) Name() string { return c.manifest.Name }

func (c *Client) handshake(ctx context.Context) error {
	timeout := c.opts.StartTimeout
	if timeout <= 0 {
		timeout = DefaultStartTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	raw, err := c.call(ctx, MethodInitialize, InitializeParams{
		Protocol:  ProtocolVersion,
		Host:      c.opts.HostVersion,
		Workspace: c.opts.Workspace,
	})
	if err != nil {
		return fmt.Errorf("plugin %s: handshake failed: %w%s", c.opts.Command[0], err, c.stderrTail())
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("plugin %s: unreadable manifest: %w", c.opts.Command[0], err)
	}
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("plugin %s: %w", c.opts.Command[0], err)
	}
	c.manifest = manifest
	return nil
}

// Call runs one of the plugin's tools.
func (c *Client) Call(ctx context.Context, name string, arguments json.RawMessage) (CallResult, error) {
	timeout := c.opts.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	raw, err := c.call(ctx, MethodToolsCall, CallParams{Name: name, Arguments: arguments})
	if err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == CodeToolFailed {
			// The tool ran and failed. That is a result the model should see,
			// not a plugin malfunction.
			return CallResult{Content: rpcErr.Message, IsError: true}, nil
		}
		return CallResult{}, fmt.Errorf("plugin %s: %s: %w%s", c.manifest.Name, name, err, c.stderrTail())
	}
	var result CallResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return CallResult{}, fmt.Errorf("plugin %s: %s returned an unreadable result: %w", c.manifest.Name, name, err)
	}
	return result, nil
}

// call sends one request and waits for its reply.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode %s params: %w", method, err)
	}
	c.nextID++
	id := c.nextID
	line, err := json.Marshal(Request{JSONRPC: "2.0", ID: &id, Method: method, Params: body})
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", method, err)
	}
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("write %s request: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	case err := <-c.readErr:
		return nil, err
	case <-c.exited:
		return nil, fmt.Errorf("%s: plugin exited before replying", method)
	case resp := <-c.responses:
		if resp.ID == nil || *resp.ID != id {
			return nil, fmt.Errorf("%s: reply carried id %v, expected %d", method, resp.ID, id)
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// readLoop turns the plugin's stdout into responses.
func (c *Client) readLoop(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var resp Response
		if err := json.Unmarshal(raw, &resp); err != nil {
			c.failRead(fmt.Errorf("plugin wrote a line that is not JSON-RPC: %w", err))
			return
		}
		c.responses <- resp
	}
	if err := sc.Err(); err != nil {
		c.failRead(fmt.Errorf("reading from plugin: %w", err))
	}
}

func (c *Client) failRead(err error) {
	select {
	case c.readErr <- err:
	default:
	}
}

// drainStderr keeps the plugin's last words for diagnostics. Without this a
// crashed plugin produces a bare EOF and no explanation.
func (c *Client) drainStderr(stderr io.Reader) {
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 0, 8<<10), 1<<20)
	for sc.Scan() {
		c.stderrMu.Lock()
		c.stderr = append(c.stderr, sc.Text())
		if len(c.stderr) > stderrKeep {
			c.stderr = c.stderr[len(c.stderr)-stderrKeep:]
		}
		c.stderrMu.Unlock()
	}
}

// stderrTail renders recent plugin stderr for an error message.
func (c *Client) stderrTail() string {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	if len(c.stderr) == 0 {
		return ""
	}
	return "\nplugin stderr:\n  " + strings.Join(c.stderr, "\n  ")
}

// Close shuts the plugin down, politely first.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		// A shutdown notification lets the plugin flush and exit on its own.
		if line, err := json.Marshal(Request{JSONRPC: "2.0", Method: MethodShutdown}); err == nil {
			_, _ = c.stdin.Write(append(line, '\n'))
		}
		_ = c.stdin.Close()

		select {
		case <-c.exited:
		case <-time.After(3 * time.Second):
			// It had its chance.
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			<-c.exited
		}
	})
	return c.closeErr
}
