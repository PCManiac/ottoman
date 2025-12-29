package ottoman

import (
	"context"
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
	Timeout       int `env:"JS_TIMEOUT" envDefault:"2"`
	MaxConcurrent int `env:"JS_MAX_CONCURRENT" envDefault:"50"`
}

var (
	once          sync.Once
	semaphore     chan struct{}
	globalConfig  otto_config
	configChecked bool
	configMutex   sync.Mutex
)

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
			// Use defaults if env parse fails
			cfg.MaxConcurrent = 50
			cfg.Timeout = 2
		}
		globalConfig = cfg
		semaphore = make(chan struct{}, cfg.MaxConcurrent)
		configChecked = true
	}
}

func ProcessRequest(script string, params map[string]interface{}) (response map[string]interface{}, err error) {
	// Initialize semaphore and config once
	once.Do(initSemaphore)

	// Validate input
	if script == "" {
		return nil, fmt.Errorf("script cannot be empty")
	}
	if params == nil {
		return nil, fmt.Errorf("params cannot be nil")
	}

	// Acquire semaphore to limit concurrent executions
	select {
	case semaphore <- struct{}{}:
		defer func() { <-semaphore }() // Release semaphore when done
	case <-time.After(30 * time.Second):
		// Timeout if too many concurrent requests
		return nil, fmt.Errorf("too many concurrent JavaScript executions, please try again later")
	}

	// Use cached config instead of parsing every time
	cfg := globalConfig

	vm := otto.New()

	err = jsRegisterFunctions(vm)
	if err != nil {
		return nil, fmt.Errorf("otto registreing standart functions error: %w", err)
	}

	for key, uf := range params {
		err = vm.Set(key, uf)
		if err != nil {
			return nil, fmt.Errorf("otto setting variables error: %w", err)
		}
	}

	vm.Interrupt = make(chan func(), 1)
	timeoutCtx, timeoutCancel := context.WithCancel(context.Background())
	timeoutTimer := time.NewTimer(time.Duration(cfg.Timeout) * time.Second)

	// Start timeout goroutine
	go func() {
		select {
		case <-timeoutTimer.C:
			// Timer expired, try to interrupt (non-blocking)
			select {
			case vm.Interrupt <- func() {
				panic(errors.New("some code took to long! Stopping after timeout"))
			}:
				// Interrupt sent successfully
			case <-timeoutCtx.Done():
				// Script already completed, cancel prevented interrupt
			default:
				// Can't send interrupt (channel full), but context cancelled
				// Just exit - script might be done anyway
			}
		case <-timeoutCtx.Done():
			// Script completed before timeout, stop timer
			if !timeoutTimer.Stop() {
				// Timer already fired, drain it
				select {
				case <-timeoutTimer.C:
				default:
				}
			}
		}
	}()

	defer func() {
		// Cancel timeout context to signal goroutine to exit
		timeoutCancel()

		// Stop timer - this is safe even if it already fired
		timeoutTimer.Stop()
		// Drain timer channel to prevent goroutine leak
		select {
		case <-timeoutTimer.C:
		default:
		}

		if r := recover(); r != nil {
			switch x := r.(type) {
			case error:
				err = x
			default:
				err = errors.New("otto run error")
			}
			response = nil
		}
	}()

	_, err = vm.Run(script)

	if err != nil {
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
