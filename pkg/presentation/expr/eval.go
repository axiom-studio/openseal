package expr

import (
	"math"
)

type checker struct {
	inputs map[string]Type
	scope  map[string]bool // sum index names in scope
	inSum  bool
}

func (c *checker) check(n *node) (Type, error) {
	switch n.kind {
	case nodeNumber:
		return Number, nil
	case nodeName:
		if c.scope[n.name] {
			return Number, nil
		}
		if typ, ok := c.inputs[n.name]; ok {
			return typ, nil
		}
		if _, ok := functions[n.name]; ok {
			return 0, errorf(PhaseType, n.pos, "%s is a function; call it like %s(x)", n.name, n.name)
		}
		return 0, errorf(PhaseType, n.pos, "unknown name %q; inputs are %s", n.name, quoteNames(c.inputs))
	case nodeUnary:
		want := Number
		if n.name == "!" {
			want = Bool
		}
		return want, c.expect(n.args[0], want, n.name)
	case nodeBinary:
		return c.binary(n)
	case nodeCall:
		return c.call(n)
	}
	return 0, errorf(PhaseType, n.pos, "unknown expression")
}

func (c *checker) expect(n *node, want Type, context string) error {
	got, err := c.check(n)
	if err != nil {
		return err
	}
	if got != want {
		return errorf(PhaseType, n.pos, "%s needs a %s, not a %s", context, want, got)
	}
	return nil
}

func (c *checker) binary(n *node) (Type, error) {
	switch n.name {
	case "&&", "||":
		for _, arg := range n.args {
			if err := c.expect(arg, Bool, n.name); err != nil {
				return 0, err
			}
		}
		return Bool, nil
	case "==", "!=":
		left, err := c.check(n.args[0])
		if err != nil {
			return 0, err
		}
		if err := c.expect(n.args[1], left, n.name); err != nil {
			return 0, err
		}
		return Bool, nil
	case "<", "<=", ">", ">=":
		for _, arg := range n.args {
			if err := c.expect(arg, Number, n.name); err != nil {
				return 0, err
			}
		}
		return Bool, nil
	default:
		for _, arg := range n.args {
			if err := c.expect(arg, Number, n.name); err != nil {
				return 0, err
			}
		}
		return Number, nil
	}
}

func (c *checker) call(n *node) (Type, error) {
	fn, ok := functions[n.name]
	if !ok {
		return 0, errorf(PhaseType, n.pos, "unknown function %q", n.name)
	}
	if len(n.args) < fn.minArgs || len(n.args) > fn.maxArgs {
		if fn.minArgs == fn.maxArgs {
			return 0, errorf(PhaseType, n.pos, "%s takes %d arguments, not %d", n.name, fn.minArgs, len(n.args))
		}
		return 0, errorf(PhaseType, n.pos, "%s takes %d-%d arguments, not %d", n.name, fn.minArgs, fn.maxArgs, len(n.args))
	}
	switch n.name {
	case "if":
		if err := c.expect(n.args[0], Bool, "if's condition"); err != nil {
			return 0, err
		}
		branch, err := c.check(n.args[1])
		if err != nil {
			return 0, err
		}
		return branch, c.expect(n.args[2], branch, "if's other branch")
	case "sum":
		index := n.args[0].name
		if c.inSum {
			return 0, errorf(PhaseType, n.pos, "sum cannot be nested")
		}
		if _, clash := c.inputs[index]; clash {
			return 0, errorf(PhaseType, n.args[0].pos, "sum index %q is already an input name", index)
		}
		if _, clash := functions[index]; clash {
			return 0, errorf(PhaseType, n.args[0].pos, "sum index %q is a function name", index)
		}
		for _, arg := range n.args[1:3] {
			if err := c.expect(arg, Number, "sum's range"); err != nil {
				return 0, err
			}
		}
		c.inSum, c.scope[index] = true, true
		err := c.expect(n.args[3], Number, "sum's body")
		c.inSum = false
		delete(c.scope, index)
		return Number, err
	case "round":
		if err := c.expect(n.args[0], Number, "round"); err != nil {
			return 0, err
		}
		if len(n.args) == 2 {
			digits := n.args[1]
			if digits.kind != nodeNumber || digits.num != math.Trunc(digits.num) || digits.num < 0 || digits.num > 10 {
				return 0, errorf(PhaseType, digits.pos, "round's digits must be a whole number literal from 0 to 10")
			}
		}
		return Number, nil
	default:
		for _, arg := range n.args {
			if err := c.expect(arg, Number, n.name); err != nil {
				return 0, err
			}
		}
		return Number, nil
	}
}

type value struct {
	n      float64
	b      bool
	isBool bool
}

type evaluator struct {
	env    map[string]float64
	inputs map[string]Type
	locals map[string]float64
}

var powersOfTen = [...]float64{1, 10, 100, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10}

func number(n *node, x float64) (value, error) {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return value{}, errorf(PhaseEval, n.pos, "result is not a finite number")
	}
	return value{n: x}, nil
}

func boolean(b bool) value { return value{b: b, isBool: true} }

func (e *evaluator) eval(n *node) (value, error) {
	switch n.kind {
	case nodeNumber:
		return value{n: n.num}, nil
	case nodeName:
		if x, ok := e.locals[n.name]; ok {
			return value{n: x}, nil
		}
		if e.inputs[n.name] == Bool {
			return boolean(e.env[n.name] != 0), nil
		}
		return value{n: e.env[n.name]}, nil
	case nodeUnary:
		operand, err := e.eval(n.args[0])
		if err != nil {
			return value{}, err
		}
		if n.name == "!" {
			return boolean(!operand.b), nil
		}
		return value{n: -operand.n}, nil
	case nodeBinary:
		return e.binary(n)
	default:
		return e.call(n)
	}
}

func (e *evaluator) binary(n *node) (value, error) {
	left, err := e.eval(n.args[0])
	if err != nil {
		return value{}, err
	}
	switch n.name {
	case "&&":
		if !left.b {
			return boolean(false), nil
		}
		return e.eval(n.args[1])
	case "||":
		if left.b {
			return boolean(true), nil
		}
		return e.eval(n.args[1])
	}
	right, err := e.eval(n.args[1])
	if err != nil {
		return value{}, err
	}
	a, b := left.n, right.n
	switch n.name {
	case "==":
		if left.isBool {
			return boolean(left.b == right.b), nil
		}
		return boolean(a == b), nil
	case "!=":
		if left.isBool {
			return boolean(left.b != right.b), nil
		}
		return boolean(a != b), nil
	case "<":
		return boolean(a < b), nil
	case "<=":
		return boolean(a <= b), nil
	case ">":
		return boolean(a > b), nil
	case ">=":
		return boolean(a >= b), nil
	case "+":
		return number(n, a+b)
	case "-":
		return number(n, a-b)
	case "*":
		return number(n, a*b)
	case "/":
		return number(n, a/b)
	default: // ^
		return number(n, pow(a, b))
	}
}

// pow computes integer powers by binary exponentiation so every conforming
// implementation produces identical bits.
func pow(x, y float64) float64 {
	if y != math.Trunc(y) || math.Abs(y) > 1024 {
		return math.Pow(x, y)
	}
	result, base, exponent := 1.0, x, int(math.Abs(y))
	for exponent > 0 {
		if exponent&1 == 1 {
			result *= base
		}
		exponent >>= 1
		if exponent == 0 {
			break
		}
		base *= base
	}
	if y < 0 {
		result = 1 / result
	}
	return result
}

func (e *evaluator) call(n *node) (value, error) {
	switch n.name {
	case "if":
		condition, err := e.eval(n.args[0])
		if err != nil {
			return value{}, err
		}
		if condition.b {
			return e.eval(n.args[1])
		}
		return e.eval(n.args[2])
	case "sum":
		return e.sum(n)
	}
	args := make([]float64, 0, len(n.args))
	for _, arg := range n.args {
		evaluated, err := e.eval(arg)
		if err != nil {
			return value{}, err
		}
		args = append(args, evaluated.n)
	}
	x := args[0]
	switch n.name {
	case "min", "max":
		for _, candidate := range args[1:] {
			if n.name == "min" && candidate < x || n.name == "max" && candidate > x {
				x = candidate
			}
		}
		return number(n, x)
	case "round":
		if len(args) == 2 {
			scale := powersOfTen[int(args[1])]
			return number(n, math.Round(x*scale)/scale)
		}
		return number(n, math.Round(x))
	case "floor":
		return number(n, math.Floor(x))
	case "ceil":
		return number(n, math.Ceil(x))
	case "abs":
		return number(n, math.Abs(x))
	case "sqrt":
		return number(n, math.Sqrt(x))
	case "log":
		return number(n, math.Log(x))
	default: // exp
		return number(n, math.Exp(x))
	}
}

func (e *evaluator) sum(n *node) (value, error) {
	bounds := [2]float64{}
	for index, arg := range n.args[1:3] {
		bound, err := e.eval(arg)
		if err != nil {
			return value{}, err
		}
		if bound.n != math.Trunc(bound.n) || math.Abs(bound.n) > 1e9 {
			return value{}, errorf(PhaseEval, arg.pos, "sum's range must be whole numbers")
		}
		bounds[index] = bound.n
	}
	from, to := int64(bounds[0]), int64(bounds[1])
	if to-from+1 > MaxSumIterations {
		return value{}, errorf(PhaseEval, n.pos, "sum runs %d times; the limit is %d", to-from+1, MaxSumIterations)
	}
	index := n.args[0].name
	total := 0.0
	for i := from; i <= to; i++ {
		e.locals[index] = float64(i)
		term, err := e.eval(n.args[3])
		if err != nil {
			delete(e.locals, index)
			return value{}, err
		}
		total += term.n
		if math.IsInf(total, 0) {
			delete(e.locals, index)
			return value{}, errorf(PhaseEval, n.pos, "result is not a finite number")
		}
	}
	delete(e.locals, index)
	return value{n: total}, nil
}
