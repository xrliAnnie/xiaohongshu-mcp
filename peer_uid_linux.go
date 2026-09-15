package main

import "golang.org/x/sys/unix"

func kernelPeerUID(fd int) (uint32, error) {
	credential, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return credential.Uid, nil
}
