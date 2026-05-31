package lark

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type MessageConsumer struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan MessageEvent
	errs   chan error
	done   chan error
}

func StartMessageConsumer(ctx context.Context, bin string, noProxy ...bool) (*MessageConsumer, error) {
	cli := NewCLI(bin, noProxy...)
	cmd := exec.Command(cli.Bin, "event", "consume", "im.message.receive_v1", "--as", "bot")
	if cli.NoProxy {
		cmd.Env = append(os.Environ(), "LARK_CLI_NO_PROXY=1")
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open lark event stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open lark event stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open lark event stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start lark event consumer: %w", err)
	}

	consumer := &MessageConsumer{
		cmd:    cmd,
		stdin:  stdin,
		events: make(chan MessageEvent, 16),
		errs:   make(chan error, 2),
		done:   make(chan error, 1),
	}

	ready := make(chan error, 1)
	go drainReady(stderr, ready)
	go scanMessageEvents(stdout, consumer.events, consumer.errs)
	go waitMessageConsumer(cmd, consumer.done, consumer.errs)

	select {
	case err := <-ready:
		if err != nil {
			_ = consumer.Close()
			return nil, err
		}
	case <-ctx.Done():
		_ = consumer.Close()
		return nil, ctx.Err()
	}

	return consumer, nil
}

func (consumer *MessageConsumer) Receive(ctx context.Context) (MessageEvent, error) {
	select {
	case event, ok := <-consumer.events:
		if !ok {
			return MessageEvent{}, fmt.Errorf("lark event stream closed")
		}
		return event, nil
	case err := <-consumer.errs:
		return MessageEvent{}, err
	case <-ctx.Done():
		return MessageEvent{}, ctx.Err()
	}
}

func (consumer *MessageConsumer) Close() error {
	if consumer == nil {
		return nil
	}
	if consumer.stdin != nil {
		_ = consumer.stdin.Close()
		consumer.stdin = nil
	}
	if consumer.cmd == nil {
		return nil
	}

	select {
	case err := <-consumer.done:
		consumer.cmd = nil
		return err
	case <-time.After(5 * time.Second):
		if consumer.cmd.Process != nil {
			_ = consumer.cmd.Process.Kill()
		}
		err := <-consumer.done
		consumer.cmd = nil
		return err
	}
}

func drainReady(stderr io.Reader, ready chan<- error) {
	scanner := bufio.NewScanner(stderr)
	readySent := false
	for scanner.Scan() {
		line := scanner.Text()
		if !readySent && strings.Contains(line, "[event] ready") {
			ready <- nil
			readySent = true
		}
	}
	if err := scanner.Err(); err != nil && !readySent {
		ready <- fmt.Errorf("read lark event stderr: %w", err)
		return
	}
	if !readySent {
		ready <- fmt.Errorf("lark event consumer exited before ready")
	}
}

func scanMessageEvents(stdout io.Reader, events chan<- MessageEvent, errs chan<- error) {
	defer close(events)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event MessageEvent
		line := scanner.Bytes()
		if err := json.Unmarshal(line, &event); err != nil {
			errs <- fmt.Errorf("decode lark event %q: %w", string(line), err)
			continue
		}
		events <- event
	}
	if err := scanner.Err(); err != nil {
		errs <- fmt.Errorf("read lark event stdout: %w", err)
	}
}

func waitMessageConsumer(cmd *exec.Cmd, done chan<- error, errs chan<- error) {
	err := cmd.Wait()
	done <- err
	if err != nil {
		errs <- fmt.Errorf("lark event consumer exited: %w", err)
	}
}
