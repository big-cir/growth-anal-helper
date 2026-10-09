package auth

import (
	"io"
	"os"
	"os/signal"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

// TerminalHidden reads hidden input from the controlling terminal. It refuses without a terminal and always restores it.
func TerminalHidden(stdout io.Writer) ReadHidden {
	return func(prompt string) (string, error) {
		fd := int(os.Stdin.Fd())
		var old syscall.Termios
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlGet, uintptr(unsafe.Pointer(&old))); e != 0 {
			return "", &AccountError{"Passwords can only be typed in a terminal"}
		}
		io.WriteString(stdout, prompt)
		raw := old
		raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
		raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
		raw.Cflag &^= syscall.CSIZE | syscall.PARENB
		raw.Cflag |= syscall.CS8
		raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlSet, uintptr(unsafe.Pointer(&raw)))
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
		defer func() {
			signal.Stop(sigs)
			syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlSet, uintptr(unsafe.Pointer(&old)))
			io.WriteString(stdout, "\n")
		}()
		type result struct {
			v   string
			err error
		}
		done := make(chan result, 1)
		go func() {
			value := []rune{}
			var pending []byte
			buf := make([]byte, 256)
			for {
				n, err := os.Stdin.Read(buf)
				if err != nil {
					done <- result{"", &AccountError{"Cancelled"}}
					return
				}
				pending = append(pending, buf[:n]...)
				for len(pending) > 0 {
					r, size := utf8.DecodeRune(pending)
					if r == utf8.RuneError && size == 1 && !utf8.FullRune(pending) {
						break
					}
					pending = pending[size:]
					switch {
					case r == '\r' || r == '\n':
						done <- result{string(value), nil}
						return
					case r == 3 || r == 4:
						done <- result{"", &AccountError{"Cancelled"}}
						return
					case r == 0x7f || r == '\b':
						if len(value) > 0 {
							value = value[:len(value)-1]
						}
					case r >= ' ':
						value = append(value, r)
					}
				}
			}
		}()
		select {
		case r := <-done:
			return r.v, r.err
		case <-sigs:
			return "", &AccountError{"Cancelled"}
		}
	}
}
