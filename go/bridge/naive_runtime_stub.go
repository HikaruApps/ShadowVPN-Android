//go:build !android

package bridge

import (
	"errors"

	"github.com/xtls/xray-core/core"
)

type naiveRuntime struct{}

func newNaiveRuntime(Profile) (*naiveRuntime, error) {
	return nil, errors.New("Naive/Cronet доступен только в Android-сборке")
}

func (*naiveRuntime) Start() error                         { return nil }
func (*naiveRuntime) Install(*core.Instance, string) error { return nil }
func (*naiveRuntime) Close() error                         { return nil }
