package ottoman

import (
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/caarlos0/env"
	"github.com/robertkrimen/otto"
)

type otto_config struct {
	Timeout int `env:"JS_TIMEOUT" envDefault:"2"`
}

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

func ProcessRequest(script string, params map[string]interface{}) (response map[string]interface{}, err error) {
	var cfg otto_config
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("env vars parse error: %w", err)
	}

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
	go func() {
		time.Sleep(time.Duration(cfg.Timeout) * time.Second)
		vm.Interrupt <- func() {
			panic(errors.New("some code took to long! Stopping after timeout"))
		}
	}()

	defer func() {
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

	getOttoValue := func(variable string, err error) (interface{}, error) {
		if err != nil {
			return nil, err
		}
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
		message, err := getOttoValue(v, err)
		if err != nil {
			return nil, fmt.Errorf("otto get value error: %w", err)
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
				response[v] = []byte(message.([]byte))
			default:
				response[v] = message
				//response[v] = nil
			}

		}
	}

	return
}
