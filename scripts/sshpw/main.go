package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: sshpw <host:port> <user> [command]")
		fmt.Fprintln(os.Stderr, "  password is read from $SSHPW_PASSWORD")
		fmt.Fprintln(os.Stderr, "  if no command is given, stdin is executed as a bash script")
		os.Exit(2)
	}
	addr := os.Args[1]
	user := os.Args[2]
	pass := os.Getenv("SSHPW_PASSWORD")
	if pass == "" {
		log.Fatal("SSHPW_PASSWORD not set")
	}

	var cmd string
	if len(os.Args) >= 4 {
		cmd = os.Args[3]
	} else {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatal(err)
		}
		cmd = "bash -s <<'__SSHPW_EOF__'\n" + string(b) + "\n__SSHPW_EOF__\n"
	}

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		log.Fatalf("ssh dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		log.Fatalf("ssh session: %v", err)
	}
	defer sess.Close()

	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr
	if err := sess.Run(cmd); err != nil {
		if ee, ok := err.(*ssh.ExitError); ok {
			os.Exit(ee.ExitStatus())
		}
		log.Fatalf("ssh run: %v", err)
	}
}
