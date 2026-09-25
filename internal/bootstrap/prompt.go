package bootstrap

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

type prompter struct {
	in          *bufio.Reader
	out         io.Writer
	interactive bool
}

func newPrompter(in io.Reader, out io.Writer, interactive bool) prompter {
	if in == nil {
		in = strings.NewReader("")
	}
	return prompter{in: bufio.NewReader(in), out: out, interactive: interactive}
}

func (p prompter) note(format string, args ...any) {
	if p.out == nil {
		return
	}
	_, _ = fmt.Fprintf(p.out, "tama-link: "+format+"\n", args...)
}

func (p prompter) line(ctx context.Context, label string) (string, error) {
	if !p.interactive {
		return "", usageErr(fmt.Errorf("login requires %s when input is not a terminal", label))
	}
	if err := ctx.Err(); err != nil {
		return "", cancelled()
	}
	if p.out != nil {
		_, _ = fmt.Fprint(p.out, label)
	}
	text, err := p.in.ReadString('\n')
	if err != nil {
		if err == io.EOF && strings.TrimSpace(text) == "" {
			return "", cancelled()
		}
		if err == io.EOF {
			return strings.TrimSpace(text), nil
		}
		return "", failErr(fmt.Errorf("read %s: %w", label, err))
	}
	return strings.TrimSpace(text), nil
}

func (p prompter) ask(ctx context.Context, label, fallback string) (string, error) {
	prompt := label
	if fallback != "" {
		prompt = fmt.Sprintf("%s [%s]: ", label, fallback)
	} else if !strings.HasSuffix(label, " ") && !strings.HasSuffix(label, ": ") {
		prompt = label + ": "
	}
	got, err := p.line(ctx, prompt)
	if err != nil {
		return "", err
	}
	if got == "" {
		return fallback, nil
	}
	return got, nil
}

func (p prompter) confirm(ctx context.Context, label string, defYes bool) (bool, error) {
	suffix := " [y/N]"
	if defYes {
		suffix = " [Y/n]"
	}
	got, err := p.line(ctx, label+suffix+" ")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(got) {
	case "":
		return defYes, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, usageErr(fmt.Errorf("answer yes or no"))
	}
}

func (p prompter) choose(ctx context.Context, label string, options []string) (int, error) {
	if !p.interactive {
		return 0, usageErr(fmt.Errorf("login requires an explicit choice when input is not a terminal"))
	}
	if p.out != nil {
		_, _ = fmt.Fprintln(p.out, label)
		for i, option := range options {
			_, _ = fmt.Fprintf(p.out, "  %d. %s\n", i+1, option)
		}
	}
	got, err := p.line(ctx, "Choice: ")
	if err != nil {
		return 0, err
	}
	var index int
	if _, err := fmt.Sscan(got, &index); err != nil || index < 1 || index > len(options) {
		return 0, usageErr(fmt.Errorf("choose a listed option"))
	}
	return index - 1, nil
}
