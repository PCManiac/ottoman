package ottoman

import (
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/caarlos0/env"
	"github.com/robertkrimen/otto"
)

type otto_config struct {
	Timeout       time.Duration `env:"JS_TIMEOUT" envDefault:"2s"`
	MaxConcurrent int           `env:"JS_MAX_CONCURRENT" envDefault:"50"`
	WaitTimeout   time.Duration `env:"JS_MAX_WAITTIMEOUT" envDefault:"30s"`
}

var (
	once          sync.Once
	semaphore     chan struct{}
	globalConfig  otto_config
	configChecked bool
	configMutex   sync.Mutex
)

var ErrTimeout = errors.New("javascript execution timeout")

func jsBtoa(b string) string {
	return base64.StdEncoding.EncodeToString([]byte(b))
}

func jsAtob(str string) string {
	b, err := base64.StdEncoding.DecodeString(str)
	if err != nil {
		v, _ := otto.ToValue("Failed to execute 'jsAtob': The string to be decoded is not correctly encoded.")
		panic(v)
	}
	return string(b)
}

func jsRegisterFunctions(vm *otto.Otto) (err error) {
	err = vm.Set("btoa", jsBtoa)
	if err != nil {
		return err
	}
	err = vm.Set("atob", jsAtob)
	if err != nil {
		return err
	}

	return
}

func initSemaphore() {
	configMutex.Lock()
	defer configMutex.Unlock()

	if !configChecked {
		var cfg otto_config
		if err := env.Parse(&cfg); err != nil {
			cfg.MaxConcurrent = 50
			cfg.Timeout = 2 * time.Second
			cfg.WaitTimeout = 30 * time.Second
		}
		globalConfig = cfg
		semaphore = make(chan struct{}, cfg.MaxConcurrent)
		configChecked = true
	}
}

func ProcessRequest(script string, params map[string]interface{}) (response map[string]interface{}, err error) {
	once.Do(initSemaphore)

	cfg := globalConfig

	select {
	case semaphore <- struct{}{}:
		defer func() { <-semaphore }()
	case <-time.After(cfg.WaitTimeout):
		return nil, errors.New("too many concurrent JavaScript executions, please try again later")
	}

	vm := otto.New()

	err = jsRegisterFunctions(vm)
	if err != nil {
		return nil, fmt.Errorf("otto registering standard functions error: %w", err)
	}

	for key, uf := range params {
		err = vm.Set(key, uf)
		if err != nil {
			return nil, fmt.Errorf("otto setting variables error: %w", err)
		}
	}

	timer := time.AfterFunc(cfg.Timeout, func() {
		vm.Interrupt <- func() {
			panic(ErrTimeout)
		}
	})
	defer timer.Stop()

	_, err = vm.Run(script)

	if err != nil {
		if errors.Is(err, ErrTimeout) {
			return nil, ErrTimeout
		}

		return nil, fmt.Errorf("otto run error: %w", err)
	}

	getOttoValue := func(variable string) (interface{}, error) {
		value, err := vm.Get(variable)
		if err != nil {
			return nil, err
		}
		exported, err := value.Export()
		if err != nil {
			return nil, err
		}
		return exported, nil
	}

	response = make(map[string]interface{})

	for v := range params {
		message, getErr := getOttoValue(v)
		if getErr != nil {
			return nil, fmt.Errorf("otto get value error for '%s': %w", v, getErr)
		}

		switch message := message.(type) {
		case string,
			int, int8, int16, int32, int64,
			float32, float64,
			bool:
			response[v] = message
		default:
			rt := reflect.TypeOf(message)
			switch rt.Kind() {
			case reflect.Slice, reflect.Array:
				// Safe type assertion for byte slice
				if byteSlice, ok := message.([]byte); ok {
					response[v] = byteSlice
				} else {
					response[v] = message
				}
			default:
				response[v] = message
			}

		}
	}

	return
}
