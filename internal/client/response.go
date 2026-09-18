package client

import (
	"errors"
	"fmt"
	"io"
)

const maxResponseBytes int64 = 20 << 20

var ErrResponseTooLarge = errors.New("portal response exceeds the configured size limit")

func readResponseBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, fmt.Errorf("%w (%d bytes)", ErrResponseTooLarge, maxResponseBytes)
	}
	return body, nil
}
