package store

import (
	"errors"
	"os"
	"strings"
)

func validID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00")
}

func missingObject(err error) bool {
	var e interface{ NotFound() bool }
	return os.IsNotExist(err) || errors.As(err, &e) && e.NotFound()
}
