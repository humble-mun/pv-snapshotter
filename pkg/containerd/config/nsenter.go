//go:build linux

package config

/*
#cgo CFLAGS: -Wall

#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

// nsenter_preamble runs as a C constructor — before the Go runtime (and its
// extra threads) are initialised.  At this point the process is still
// single-threaded, so setns(CLONE_NEWNS) is accepted by the kernel.
//
// It is gated by the environment variable _PV_NSENTER so that normal process
// starts are unaffected.  RestartContainerd() re-execs the binary with this
// variable set, then waits for the child to finish.
__attribute__((constructor)) static void nsenter_preamble(void) {
    const char *trigger = getenv("_PV_NSENTER");
    if (!trigger || trigger[0] == '\0') {
        return;
    }

    // Enter the host mount namespace via /proc/1/ns/mnt.
    int fd = open("/proc/1/ns/mnt", O_RDONLY | O_CLOEXEC);
    if (fd < 0) {
        fprintf(stderr, "pv-snapshotter nsenter: open /proc/1/ns/mnt: %s\n",
                strerror(errno));
        exit(1);
    }
    if (setns(fd, CLONE_NEWNS) != 0) {
        fprintf(stderr, "pv-snapshotter nsenter: setns CLONE_NEWNS: %s\n",
                strerror(errno));
        close(fd);
        exit(1);
    }
    close(fd);

    // Locate systemctl on the host filesystem.
    const char *candidates[] = {
        "/usr/bin/systemctl",
        "/bin/systemctl",
        NULL,
    };
    const char *systemctl = NULL;
    for (int i = 0; candidates[i] != NULL; i++) {
        if (access(candidates[i], X_OK) == 0) {
            systemctl = candidates[i];
            break;
        }
    }
    if (!systemctl) {
        fprintf(stderr, "pv-snapshotter nsenter: systemctl not found\n");
        exit(1);
    }

    char *argv[] = {"systemctl", "restart", "containerd", NULL};
    execv(systemctl, argv);
    // execv(2) replaces the process image on success and never returns.
    // Reaching this line means execv failed — report the error and exit non-zero.
    fprintf(stderr, "pv-snapshotter nsenter: execv %s: %s\n",
            systemctl, strerror(errno));
    exit(1);
}
*/
import "C"

import (
	"fmt"
	"os"
	"os/exec"
)

const nsenterEnvKey = "_PV_NSENTER"

// RestartContainerd re-execs the current binary with _PV_NSENTER=1.
//
// The re-exec'd child runs the C constructor nsenter_preamble before the Go
// runtime starts.  At that point the process is single-threaded, so the kernel
// accepts setns(CLONE_NEWNS).  The constructor then execs systemctl directly —
// the Go main() in the child is never reached.
//
// This approach (re-exec with a gated C constructor) is the same technique
// used by runc and containerd-shim for namespace entry from Go binaries.
func RestartContainerd() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), nsenterEnvKey+"=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("containerd restart via nsenter preamble: %w", err)
	}
	return nil
}
