package idempotency

type Record struct {
	RequestHash string
	Response    any
}

type Store interface {
	Resolve(key, requestHash string, response any) (any, bool, bool, error)
}
