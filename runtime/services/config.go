package services

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"
)

type Option func(*configLoader) error

func WithFileLoader(path string) Option {
	return func(loader *configLoader) error {
		loader.secretsMountPath = path
		return nil
	}
}

func LoadConfig(dst any, opts ...Option) error {
	var loader configLoader
	for _, opt := range opts {
		if err := opt(&loader); err != nil {
			return err
		}
	}
	return loader.loadConfig(dst, nil)
}

type configLoader struct {
	secretsMountPath string
}

func (l *configLoader) loadConfig(dst any, pathPrefix []string) error {
	typ := reflect.TypeOf(dst)

	if typ.Kind() != reflect.Ptr || typ.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("expected a pointer to a struct, got %T", dst)
	}

	typ = typ.Elem()
	val := reflect.ValueOf(dst).Elem()

	for i := range typ.NumField() {
		field := typ.Field(i)

		var (
			defaultTag, hasDefault = field.Tag.Lookup("default")
			envTag                 = field.Tag.Get("env")
			nestedTag              = field.Tag.Get("nested")
		)

		if nestedTag != "" {
			if field.Type.Kind() != reflect.Struct {
				return fmt.Errorf("nested tag is only supported for struct fields: %s", field.Name)
			}
			if err := l.loadConfig(val.Field(i).Addr().Interface(), append(pathPrefix, field.Name)); err != nil {
				return err
			}
			continue
		}

		if envTag == "" && !hasDefault {
			return fmt.Errorf("missing env or secret tag for field %s and no default is set", field.Name)
		}

		value := defaultTag

		if envTag != "" {
			if l.secretsMountPath != "" {
				secretPath := strings.ToLower(strings.Join(append(pathPrefix, envTag), "/"))
				raw, err := os.ReadFile(path.Join(l.secretsMountPath, secretPath))
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("failed to read file %s: %v", secretPath, err)
				}
				if err == nil {
					value = string(raw)
				}
			}

			envName := strings.ToUpper(strings.Join(append(pathPrefix, envTag), "_"))
			if newValue, ok := os.LookupEnv(envName); ok {
				value = newValue
			} else if !hasDefault {
				return fmt.Errorf("missing env variable: %s", envName)
			}
		}

		fieldValue := val.Field(i)
		if !fieldValue.CanSet() {
			return fmt.Errorf("cannot set field %s", field.Name)
		}
		if err := setValue(fieldValue, value); err != nil {
			return fmt.Errorf("failed to set field %s: %w", field.Name, err)
		}
	}

	return nil
}

func setValue(field reflect.Value, value string) error {
	switch field.Kind() {
	case reflect.String:
		field.SetString(value)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		intValue, err := strconv.ParseInt(value, 10, field.Type().Bits())
		if err != nil {
			return fmt.Errorf("failed to convert %s to int: %v", value, err)
		}
		field.SetInt(int64(intValue))

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		uintValue, err := strconv.ParseUint(value, 10, field.Type().Bits())
		if err != nil {
			return fmt.Errorf("failed to convert %s to uint: %v", value, err)
		}
		field.SetUint(uint64(uintValue))

	case reflect.Float32, reflect.Float64:
		floatValue, err := strconv.ParseFloat(value, field.Type().Bits())
		if err != nil {
			return fmt.Errorf("failed to convert %s to float: %v", value, err)
		}
		field.SetFloat(floatValue)

	case reflect.Bool:
		boolValue, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("failed to convert %s to bool: %v", value, err)
		}
		field.SetBool(boolValue)

	case reflect.Slice:
		if field.Type().Elem().Kind() != reflect.Uint8 {
			return fmt.Errorf("unsupported slice type: %s", field.Type())
		}
		field.SetBytes([]byte(value))

	default:
		return fmt.Errorf("unsupported field type: %s", field.Type())
	}

	return nil
}

type DatabaseConfig struct {
	Host     string `env:"HOST"`
	Username string `env:"USERNAME"`
	Password string `env:"PASSWORD"`
	Name     string `env:"NAME"`
	SSLMode  string `env:"SSL_MODE" default:"disable"`
}

func (c *DatabaseConfig) ConnString() (string, error) {
	if strings.HasPrefix(c.Host, "/") {
		// support unix sockets
		return fmt.Sprintf("host=%s user=%s password=%s dbname=%s sslmode=%s", c.Host, c.Username, c.Password, c.Name, c.SSLMode), nil
	}

	host, port, err := net.SplitHostPort(c.Host)
	if err != nil {
		return "", fmt.Errorf("failed to split host and port: %w", err)
	}
	connString := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s", host, port, c.Username, c.Password, c.Name, c.SSLMode)
	return connString, nil
}
