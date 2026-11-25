package pkg

func Shared() int {
	return 1
}

func Shadow() int {
	x := 1
	_ = x
	{
		x := 2
		return x
	}
}

func Source() int {
	tainted := 1
	return Pass(tainted)
}

func Pass(v int) int {
	return Sink(v)
}

func Sink(v int) int {
	return v
}

func Closure() int {
	tainted := 1
	f := func() int { return Sink(tainted) }
	return f()
}

type Runner interface {
	Run(int) int
}

type Impl struct{}

func (Impl) Run(v int) int {
	return Sink(v)
}

func ViaIface(r Runner, tainted int) int {
	return r.Run(tainted)
}

func CopyFlow() int {
	tainted := 1
	mid := tainted
	return Sink(mid)
}

func ReturnFlow() int {
	tainted := Sink(1)
	return tainted
}

func ArgFlow() int {
	tainted := 2
	return Id(tainted)
}

func Id(v int) int {
	return v
}
