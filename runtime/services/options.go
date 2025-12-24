package services

type serivceOptions struct {
	logHandlerWrappers []logHandlerWrapper
}

type Option func(*serivceOptions) error

func WithLogHandlerWrapper(wrapper logHandlerWrapper) Option {
	return func(so *serivceOptions) error {
		so.logHandlerWrappers = append(so.logHandlerWrappers, wrapper)
		return nil
	}
}
