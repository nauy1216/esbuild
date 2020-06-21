// This file contains code for "lowering" syntax, which means converting it to
// older JavaScript. For example, "a ** b" becomes a call to "Math.pow(a, b)"
// when lowered. Which syntax is lowered is determined by the language target.

package parser

import (
	"fmt"

	"github.com/evanw/esbuild/internal/ast"
	"github.com/evanw/esbuild/internal/lexer"
)

type futureSyntax uint8

const (
	futureSyntaxAsync futureSyntax = iota
	futureSyntaxAsyncGenerator
	futureSyntaxRestProperty
	futureSyntaxForAwait
	futureSyntaxBigInteger
	futureSyntaxNonIdentifierArrayRest
	futureSyntaxPrivateName
)

func (p *parser) markFutureSyntax(syntax futureSyntax, r ast.Range) {
	var target LanguageTarget

	switch syntax {
	case futureSyntaxAsync:
		target = ES2017
	case futureSyntaxAsyncGenerator:
		target = ES2018
	case futureSyntaxRestProperty:
		target = ES2018
	case futureSyntaxForAwait:
		target = ES2018
	case futureSyntaxBigInteger:
		target = ES2020
	case futureSyntaxNonIdentifierArrayRest:
		target = ES2016
	case futureSyntaxPrivateName:
		target = ESNext
	}

	if p.Target < target {
		var name string
		yet := " yet"

		switch syntax {
		case futureSyntaxAsync:
			name = "Async functions"
		case futureSyntaxAsyncGenerator:
			name = "Async generator functions"
		case futureSyntaxRestProperty:
			name = "Rest properties"
		case futureSyntaxForAwait:
			name = "For-await loops"
		case futureSyntaxBigInteger:
			name = "Big integer literals"
			yet = "" // This will never be supported
		case futureSyntaxNonIdentifierArrayRest:
			name = "Non-identifier array rest patterns"
		case futureSyntaxPrivateName:
			name = "Private names"
		}

		p.log.AddRangeError(&p.source, r,
			fmt.Sprintf("%s are from %s and transforming them to %s is not supported%s",
				name, targetTable[target], targetTable[p.Target], yet))
	}
}

func (p *parser) lowerOptionalChain(expr ast.Expr, in exprIn, out exprOut, thisArgFunc func() ast.Expr) (ast.Expr, exprOut) {
	valueWhenUndefined := ast.Expr{expr.Loc, &ast.EUndefined{}}
	endsWithPropertyAccess := false
	startsWithCall := false
	originalExpr := expr
	chain := []ast.Expr{}
	loc := expr.Loc

	// Step 1: Get an array of all expressions in the chain. We're traversing the
	// chain from the outside in, so the array will be filled in "backwards".
flatten:
	for {
		chain = append(chain, expr)

		switch e := expr.Data.(type) {
		case *ast.EDot:
			expr = e.Target
			if len(chain) == 1 {
				endsWithPropertyAccess = true
			}
			if e.OptionalChain == ast.OptionalChainStart {
				break flatten
			}

		case *ast.EIndex:
			expr = e.Target
			if len(chain) == 1 {
				endsWithPropertyAccess = true
			}
			if e.OptionalChain == ast.OptionalChainStart {
				break flatten
			}

		case *ast.ECall:
			expr = e.Target
			if e.OptionalChain == ast.OptionalChainStart {
				startsWithCall = true
				break flatten
			}

		case *ast.EUnary: // UnOpDelete
			valueWhenUndefined = ast.Expr{loc, &ast.EBoolean{Value: true}}
			expr = e.Value

		default:
			panic("Internal error")
		}
	}

	// Stop now if we can strip the whole chain as dead code. Since the chain is
	// lazily evaluated, it's safe to just drop the code entirely.
	switch expr.Data.(type) {
	case *ast.ENull, *ast.EUndefined:
		return valueWhenUndefined, exprOut{}
	}

	// Don't lower this if we don't need to. This check must be done here instead
	// of earlier so we can do the dead code elimination above when the target is
	// null or undefined.
	if p.Target >= ES2020 {
		return originalExpr, exprOut{}
	}

	// Step 2: Figure out if we need to capture the value for "this" for the
	// initial ECall. This will be passed to ".call(this, ...args)" later.
	var thisArg ast.Expr
	var targetWrapFunc func(ast.Expr) ast.Expr
	if startsWithCall {
		if thisArgFunc != nil {
			// The initial value is a nested optional chain that ended in a property
			// access. The nested chain was processed first and has saved the
			// appropriate value for "this". The callback here will return a
			// reference to that saved location.
			thisArg = thisArgFunc()
		} else {
			// The initial value is a normal expression. If it's a property access,
			// strip the property off and save the target of the property access to
			// be used as the value for "this".
			switch e := expr.Data.(type) {
			case *ast.EDot:
				targetFunc, wrapFunc := p.captureValueWithPossibleSideEffects(loc, 2, e.Target)
				expr = ast.Expr{loc, &ast.EDot{
					Target:  targetFunc(),
					Name:    e.Name,
					NameLoc: e.NameLoc,
				}}
				thisArg = targetFunc()
				targetWrapFunc = wrapFunc

			case *ast.EIndex:
				targetFunc, wrapFunc := p.captureValueWithPossibleSideEffects(loc, 2, e.Target)
				expr = ast.Expr{loc, &ast.EIndex{
					Target: targetFunc(),
					Index:  e.Index,
				}}
				thisArg = targetFunc()
				targetWrapFunc = wrapFunc
			}
		}
	}

	// Step 3: Figure out if we need to capture the starting value. We don't need
	// to capture it if it doesn't have any side effects (e.g. it's just a bare
	// identifier). Skipping the capture reduces code size and matches the output
	// of the TypeScript compiler.
	exprFunc, exprWrapFunc := p.captureValueWithPossibleSideEffects(loc, 2, expr)
	expr = exprFunc()
	result := exprFunc()

	// Step 4: Wrap the starting value by each expression in the chain. We
	// traverse the chain in reverse because we want to go from the inside out
	// and the chain was built from the outside in.
	for i := len(chain) - 1; i >= 0; i-- {
		// Save a reference to the value of "this" for our parent ECall
		if i == 0 && in.storeThisArgForParentOptionalChain != nil && endsWithPropertyAccess {
			result = in.storeThisArgForParentOptionalChain(result)
		}

		switch e := chain[i].Data.(type) {
		case *ast.EDot:
			result = ast.Expr{loc, &ast.EDot{
				Target:  result,
				Name:    e.Name,
				NameLoc: e.NameLoc,
			}}

		case *ast.EIndex:
			result = ast.Expr{loc, &ast.EIndex{
				Target: result,
				Index:  e.Index,
			}}

		case *ast.ECall:
			// If this is the initial ECall in the chain and it's being called off of
			// a property access, invoke the function using ".call(this, ...args)" to
			// explicitly provide the value for "this".
			if i == len(chain)-1 && thisArg.Data != nil {
				result = ast.Expr{loc, &ast.ECall{
					Target: ast.Expr{loc, &ast.EDot{
						Target:  result,
						Name:    "call",
						NameLoc: loc,
					}},
					Args: append([]ast.Expr{thisArg}, e.Args...),
				}}
				break
			}

			result = ast.Expr{loc, &ast.ECall{
				Target:       result,
				Args:         e.Args,
				IsDirectEval: e.IsDirectEval,
			}}

		case *ast.EUnary:
			result = ast.Expr{loc, &ast.EUnary{
				Op:    ast.UnOpDelete,
				Value: result,
			}}

		default:
			panic("Internal error")
		}
	}

	// Step 5: Wrap it all in a conditional that returns the chain or the default
	// value if the initial value is null/undefined. The default value is usually
	// "undefined" but is "true" if the chain ends in a "delete" operator.
	result = ast.Expr{loc, &ast.EIf{
		Test: ast.Expr{loc, &ast.EBinary{
			Op:    ast.BinOpLooseEq,
			Left:  expr,
			Right: ast.Expr{loc, &ast.ENull{}},
		}},
		Yes: valueWhenUndefined,
		No:  result,
	}}
	if exprWrapFunc != nil {
		result = exprWrapFunc(result)
	}
	if targetWrapFunc != nil {
		result = targetWrapFunc(result)
	}
	return result, exprOut{}
}

func (p *parser) lowerAssignmentOperator(value ast.Expr, callback func(ast.Expr, ast.Expr) ast.Expr) ast.Expr {
	switch left := value.Data.(type) {
	case *ast.EDot:
		if left.OptionalChain == ast.OptionalChainNone {
			referenceFunc, wrapFunc := p.captureValueWithPossibleSideEffects(value.Loc, 2, left.Target)
			return wrapFunc(callback(
				ast.Expr{value.Loc, &ast.EDot{
					Target:  referenceFunc(),
					Name:    left.Name,
					NameLoc: left.NameLoc,
				}},
				ast.Expr{value.Loc, &ast.EDot{
					Target:  referenceFunc(),
					Name:    left.Name,
					NameLoc: left.NameLoc,
				}},
			))
		}

	case *ast.EIndex:
		if left.OptionalChain == ast.OptionalChainNone {
			targetFunc, targetWrapFunc := p.captureValueWithPossibleSideEffects(value.Loc, 2, left.Target)
			indexFunc, indexWrapFunc := p.captureValueWithPossibleSideEffects(value.Loc, 2, left.Index)
			return targetWrapFunc(indexWrapFunc(callback(
				ast.Expr{value.Loc, &ast.EIndex{
					Target: targetFunc(),
					Index:  indexFunc(),
				}},
				ast.Expr{value.Loc, &ast.EIndex{
					Target: targetFunc(),
					Index:  indexFunc(),
				}},
			)))
		}

	case *ast.EIdentifier:
		return callback(
			ast.Expr{value.Loc, &ast.EIdentifier{left.Ref}},
			value,
		)
	}

	// We shouldn't get here with valid syntax? Just let this through for now
	// since there's currently no assignment target validation. Garbage in,
	// garbage out.
	return value
}

func (p *parser) lowerExponentiationAssignmentOperator(loc ast.Loc, e *ast.EBinary) ast.Expr {
	return p.lowerAssignmentOperator(e.Left, func(a ast.Expr, b ast.Expr) ast.Expr {
		// "a **= b" => "a = __pow(a, b)"
		return ast.Expr{loc, &ast.EBinary{
			Op:    ast.BinOpAssign,
			Left:  a,
			Right: p.callRuntime(loc, "__pow", []ast.Expr{b, e.Right}),
		}}
	})
}

func (p *parser) lowerNullishCoalescingAssignmentOperator(loc ast.Loc, e *ast.EBinary) ast.Expr {
	return p.lowerAssignmentOperator(e.Left, func(a ast.Expr, b ast.Expr) ast.Expr {
		if p.Target < ES2020 {
			// "a ??= b" => "(_a = a) != null ? _a : a = b"
			testFunc, testWrapFunc := p.captureValueWithPossibleSideEffects(a.Loc, 2, a)
			return testWrapFunc(ast.Expr{loc, &ast.EIf{
				Test: ast.Expr{loc, &ast.EBinary{
					Op:    ast.BinOpLooseNe,
					Left:  testFunc(),
					Right: ast.Expr{loc, &ast.ENull{}},
				}},
				Yes: testFunc(),
				No:  ast.Expr{loc, &ast.EBinary{Op: ast.BinOpAssign, Left: b, Right: e.Right}},
			}})
		}

		// "a ??= b" => "a ?? (a = b)"
		return ast.Expr{loc, &ast.EBinary{
			Op:    ast.BinOpNullishCoalescing,
			Left:  a,
			Right: ast.Expr{loc, &ast.EBinary{Op: ast.BinOpAssign, Left: b, Right: e.Right}},
		}}
	})
}

func (p *parser) lowerLogicalAssignmentOperator(loc ast.Loc, e *ast.EBinary, op ast.OpCode) ast.Expr {
	return p.lowerAssignmentOperator(e.Left, func(a ast.Expr, b ast.Expr) ast.Expr {
		// "a &&= b" => "a && (a = b)"
		// "a ||= b" => "a || (a = b)"
		return ast.Expr{loc, &ast.EBinary{
			Op:    op,
			Left:  a,
			Right: ast.Expr{loc, &ast.EBinary{Op: ast.BinOpAssign, Left: b, Right: e.Right}},
		}}
	})
}

// Lower object spread for environments that don't support them. Non-spread
// properties are grouped into object literals and then passed to __assign()
// like this (__assign() is an alias for Object.assign()):
//
//   "{a, b, ...c, d, e}" => "__assign(__assign(__assign({a, b}, c), {d, e})"
//
// If the object literal starts with a spread, then we pass an empty object
// literal to __assign() to make sure we clone the object:
//
//   "{...a, b}" => "__assign(__assign({}, a), {b})"
//
// It's not immediately obvious why we don't compile everything to a single
// call to __assign(). After all, Object.assign() can take any number of
// arguments. The reason is to preserve the order of side effects. Consider
// this code:
//
//   let a = {get x() { b = {y: 2}; return 1 }}
//   let b = {}
//   let c = {...a, ...b}
//
// Converting the above code to "let c = __assign({}, a, b)" means "c" becomes
// "{x: 1}" which is incorrect. Converting the above code instead to
// "let c = __assign(__assign({}, a), b)" means "c" becomes "{x: 1, y: 2}"
// which is correct.
func (p *parser) lowerObjectSpread(loc ast.Loc, e *ast.EObject) ast.Expr {
	needsLowering := false

	if p.Target < ES2018 {
		for _, property := range e.Properties {
			if property.Kind == ast.PropertySpread {
				needsLowering = true
				break
			}
		}
	}

	if !needsLowering {
		return ast.Expr{loc, e}
	}

	var result ast.Expr
	properties := []ast.Property{}

	for _, property := range e.Properties {
		if property.Kind != ast.PropertySpread {
			properties = append(properties, property)
			continue
		}

		if len(properties) > 0 || result.Data == nil {
			if result.Data == nil {
				// "{a, ...b}" => "__assign({a}, b)"
				result = ast.Expr{loc, &ast.EObject{
					Properties:   properties,
					IsSingleLine: e.IsSingleLine,
				}}
			} else {
				// "{...a, b, ...c}" => "__assign(__assign(__assign({}, a), {b}), c)"
				result = p.callRuntime(loc, "__assign",
					[]ast.Expr{result, ast.Expr{loc, &ast.EObject{
						Properties:   properties,
						IsSingleLine: e.IsSingleLine,
					}}})
			}
			properties = []ast.Property{}
		}

		// "{a, ...b}" => "__assign({a}, b)"
		result = p.callRuntime(loc, "__assign", []ast.Expr{result, *property.Value})
	}

	if len(properties) > 0 {
		// "{...a, b}" => "__assign(__assign({}, a), {b})"
		result = p.callRuntime(loc, "__assign", []ast.Expr{result, ast.Expr{loc, &ast.EObject{
			Properties:   properties,
			IsSingleLine: e.IsSingleLine,
		}}})
	}

	return result
}

// Lower class fields for environments that don't support them. This either
// takes a statement or an expression.
func (p *parser) lowerClass(stmt ast.Stmt, expr ast.Expr) ([]ast.Stmt, ast.Expr) {
	type classKind uint8
	const (
		classKindExpr classKind = iota
		classKindStmt
		classKindExportStmt
		classKindExportDefaultStmt
	)

	// Unpack the class from the statement or expression
	var kind classKind
	var class *ast.Class
	var classLoc ast.Loc
	var defaultName ast.LocRef
	if stmt.Data == nil {
		e, _ := expr.Data.(*ast.EClass)
		class = &e.Class
		kind = classKindExpr
	} else if s, ok := stmt.Data.(*ast.SClass); ok {
		class = &s.Class
		if s.IsExport {
			kind = classKindExportStmt
		} else {
			kind = classKindStmt
		}
	} else {
		s, _ := stmt.Data.(*ast.SExportDefault)
		s2, _ := s.Value.Stmt.Data.(*ast.SClass)
		class = &s2.Class
		defaultName = s.DefaultName
		kind = classKindExportDefaultStmt
	}

	// We always lower class fields when parsing TypeScript since class fields in
	// TypeScript don't follow the JavaScript spec. We also need to always lower
	// TypeScript-style decorators since they don't have a JavaScript equivalent.
	if !p.TS.Parse && p.Target >= ESNext {
		if kind == classKindExpr {
			return nil, expr
		} else {
			return []ast.Stmt{stmt}, ast.Expr{}
		}
	}

	var ctor *ast.EFunction
	var parameterFields []ast.Stmt
	var instanceFields []ast.Stmt
	end := 0

	// These expressions are generated after the class body, in this order
	var computedPropertyCache ast.Expr
	var staticFields []ast.Expr
	var instanceDecorators []ast.Expr
	var staticDecorators []ast.Expr

	// These are only for class expressions that need to be captured
	var nameFunc func() ast.Expr
	var wrapFunc func(ast.Expr) ast.Expr

	// Class statements can be missing a name if they are in an
	// "export default" statement:
	//
	//   export default class {
	//     static foo = 123
	//   }
	//
	if kind != classKindExpr {
		nameFunc = func() ast.Expr {
			if class.Name == nil {
				if kind == classKindExportDefaultStmt {
					class.Name = &defaultName
				} else {
					class.Name = &ast.LocRef{classLoc, p.generateTempRef(tempRefNoDeclare)}
				}
			}
			p.recordUsage(class.Name.Ref)
			return ast.Expr{classLoc, &ast.EIdentifier{class.Name.Ref}}
		}
	}

	for _, prop := range class.Properties {
		// Merge parameter decorators with method decorators
		if p.TS.Parse && prop.IsMethod {
			if fn, ok := prop.Value.Data.(*ast.EFunction); ok {
				for i, arg := range fn.Fn.Args {
					for _, decorator := range arg.TSDecorators {
						// Generate a call to "__param()" for this parameter decorator
						prop.TSDecorators = append(prop.TSDecorators,
							p.callRuntime(decorator.Loc, "__param", []ast.Expr{
								ast.Expr{decorator.Loc, &ast.ENumber{float64(i)}},
								decorator,
							}),
						)
					}
				}
			}
		}

		// Make sure the order of computed property keys doesn't change. These
		// expressions have side effects and must be evaluated in order.
		keyExprNoSideEffects := prop.Key
		if prop.IsComputed && (p.TS.Parse || computedPropertyCache.Data != nil ||
			(!prop.IsMethod && p.Target < ESNext) || len(prop.TSDecorators) > 0) {
			needsKey := true

			// The TypeScript class field transform requires removing fields without
			// initializers. If the field is removed, then we only need the key for
			// its side effects and we don't need a temporary reference for the key.
			if len(prop.TSDecorators) == 0 && (prop.IsMethod || (p.TS.Parse && prop.Initializer == nil)) {
				needsKey = false
			}

			if !needsKey {
				// Just evaluate the key for its side effects
				computedPropertyCache = maybeJoinWithComma(computedPropertyCache, prop.Key)
			} else {
				// Store the key in a temporary so we can assign to it later
				ref := p.generateTempRef(tempRefNeedsDeclare)
				computedPropertyCache = maybeJoinWithComma(computedPropertyCache, ast.Expr{prop.Key.Loc, &ast.EBinary{
					Op:    ast.BinOpAssign,
					Left:  ast.Expr{prop.Key.Loc, &ast.EIdentifier{ref}},
					Right: prop.Key,
				}})
				prop.Key = ast.Expr{prop.Key.Loc, &ast.EIdentifier{ref}}
				keyExprNoSideEffects = prop.Key
			}

			// If this is a computed method, the property value will be used
			// immediately. In this case we inline all computed properties so far to
			// make sure all computed properties before this one are evaluated first.
			if prop.IsMethod {
				prop.Key = computedPropertyCache
				computedPropertyCache = ast.Expr{}
			}
		}

		// Handle decorators
		if p.TS.Parse {
			// Generate a single call to "__decorate()" for this property
			if len(prop.TSDecorators) > 0 {
				loc := prop.Key.Loc

				// Clone the key for the property descriptor
				var descriptorKey ast.Expr
				switch k := keyExprNoSideEffects.Data.(type) {
				case *ast.ENumber:
					descriptorKey = ast.Expr{loc, &ast.ENumber{k.Value}}
				case *ast.EString:
					descriptorKey = ast.Expr{loc, &ast.EString{k.Value}}
				case *ast.EIdentifier:
					descriptorKey = ast.Expr{loc, &ast.EIdentifier{k.Ref}}
				default:
					panic("Internal error")
				}

				// This code tells "__decorate()" if the descriptor should be undefined
				descriptorKind := float64(1)
				if !prop.IsMethod {
					descriptorKind = 2
				}

				decorator := p.callRuntime(loc, "__decorate", []ast.Expr{
					ast.Expr{loc, &ast.EArray{Items: prop.TSDecorators}},
					ast.Expr{loc, &ast.EDot{
						Target:  nameFunc(),
						Name:    "prototype",
						NameLoc: loc,
					}},
					descriptorKey,
					ast.Expr{loc, &ast.ENumber{descriptorKind}},
				})

				// Static decorators are grouped after instance decorators
				if prop.IsStatic {
					staticDecorators = append(staticDecorators, decorator)
				} else {
					instanceDecorators = append(instanceDecorators, decorator)
				}
			}
		}

		// Instance and static fields are a JavaScript feature
		if (p.TS.Parse || p.Target < ESNext) && !prop.IsMethod && (prop.IsStatic || prop.Value == nil) {
			_, isPrivateField := prop.Key.Data.(*ast.EPrivateIdentifier)

			// The TypeScript compiler doesn't follow the JavaScript spec for
			// uninitialized fields. They are supposed to be set to undefined but the
			// TypeScript compiler just omits them entirely.
			if !p.TS.Parse || prop.Initializer != nil || prop.Value != nil {
				// Determine where to store the field
				var target ast.Expr
				if prop.IsStatic {
					if nameFunc == nil {
						// If this is a class expression, capture and store it. We have to
						// do this even if it has a name since the name isn't exposed
						// outside the class body.
						classExpr := &ast.EClass{Class: *class}
						class = &classExpr.Class
						nameFunc, wrapFunc = p.captureValueWithPossibleSideEffects(classLoc, 2, ast.Expr{classLoc, classExpr})
						expr = nameFunc()
					}
					target = nameFunc()
				} else {
					target = ast.Expr{prop.Key.Loc, &ast.EThis{}}
				}

				// Generate the assignment target
				if key, ok := prop.Key.Data.(*ast.EString); ok && !prop.IsComputed {
					target = ast.Expr{prop.Key.Loc, &ast.EDot{
						Target:  target,
						Name:    lexer.UTF16ToString(key.Value),
						NameLoc: prop.Key.Loc,
					}}
				} else {
					target = ast.Expr{prop.Key.Loc, &ast.EIndex{
						Target: target,
						Index:  prop.Key,
					}}
				}

				// Generate the assignment initializer
				var init ast.Expr
				if prop.Initializer != nil {
					init = *prop.Initializer
				} else if prop.Value != nil {
					init = *prop.Value
				} else {
					init = ast.Expr{prop.Key.Loc, &ast.EUndefined{}}
				}

				expr := ast.Expr{prop.Key.Loc, &ast.EBinary{ast.BinOpAssign, target, init}}
				if prop.IsStatic {
					// Move this property to an assignment after the class ends
					staticFields = append(staticFields, expr)
				} else {
					// Move this property to an assignment inside the class constructor
					instanceFields = append(instanceFields, ast.Stmt{prop.Key.Loc, &ast.SExpr{expr}})
				}
			}

			if isPrivateField {
				// Keep the private field but remove the initializer
				prop.Initializer = nil
			} else {
				// Remove the field from the class body
				continue
			}
		}

		// Remember where the constructor is for later
		if prop.IsMethod && prop.Value != nil {
			if str, ok := prop.Key.Data.(*ast.EString); ok && lexer.UTF16EqualsString(str.Value, "constructor") {
				if fn, ok := prop.Value.Data.(*ast.EFunction); ok {
					ctor = fn

					// Initialize TypeScript constructor parameter fields
					if p.TS.Parse {
						for _, arg := range ctor.Fn.Args {
							if arg.IsTypeScriptCtorField {
								if id, ok := arg.Binding.Data.(*ast.BIdentifier); ok {
									parameterFields = append(parameterFields, ast.Stmt{arg.Binding.Loc, &ast.SExpr{ast.Expr{arg.Binding.Loc, &ast.EBinary{
										ast.BinOpAssign,
										ast.Expr{arg.Binding.Loc, &ast.EDot{
											Target:  ast.Expr{arg.Binding.Loc, &ast.EThis{}},
											Name:    p.symbols[id.Ref.InnerIndex].Name,
											NameLoc: arg.Binding.Loc,
										}},
										ast.Expr{arg.Binding.Loc, &ast.EIdentifier{id.Ref}},
									}}}})
								}
							}
						}
					}
				}
			}
		}

		// Keep this property
		class.Properties[end] = prop
		end++
	}

	// Finish the filtering operation
	class.Properties = class.Properties[:end]

	// Insert instance field initializers into the constructor
	if len(instanceFields) > 0 || len(parameterFields) > 0 {
		// Create a constructor if one doesn't already exist
		if ctor == nil {
			ctor = &ast.EFunction{}

			// Append it to the list to reuse existing allocation space
			class.Properties = append(class.Properties, ast.Property{
				IsMethod: true,
				Key:      ast.Expr{classLoc, &ast.EString{lexer.StringToUTF16("constructor")}},
				Value:    &ast.Expr{classLoc, ctor},
			})

			// Make sure the constructor has a super() call if needed
			if class.Extends != nil {
				argumentsRef := p.newSymbol(ast.SymbolUnbound, "arguments")
				p.currentScope.Generated = append(p.currentScope.Generated, argumentsRef)
				ctor.Fn.Body.Stmts = append(ctor.Fn.Body.Stmts, ast.Stmt{classLoc, &ast.SExpr{ast.Expr{classLoc, &ast.ECall{
					Target: ast.Expr{classLoc, &ast.ESuper{}},
					Args: []ast.Expr{
						ast.Expr{classLoc, &ast.ESpread{ast.Expr{classLoc, &ast.EIdentifier{argumentsRef}}}},
					},
				}}}})
			}
		}

		// Insert the instance field initializers after the super call if there is one
		stmtsFrom := ctor.Fn.Body.Stmts
		stmtsTo := []ast.Stmt{}
		if len(stmtsFrom) > 0 && ast.IsSuperCall(stmtsFrom[0]) {
			stmtsTo = append(stmtsTo, stmtsFrom[0])
			stmtsFrom = stmtsFrom[1:]
		}
		stmtsTo = append(stmtsTo, parameterFields...)
		stmtsTo = append(stmtsTo, instanceFields...)
		ctor.Fn.Body.Stmts = append(stmtsTo, stmtsFrom...)

		// Sort the constructor first to match the TypeScript compiler's output
		for i := 0; i < len(class.Properties); i++ {
			if class.Properties[i].Value != nil && class.Properties[i].Value.Data == ctor {
				ctorProp := class.Properties[i]
				for j := i; j > 0; j-- {
					class.Properties[j] = class.Properties[j-1]
				}
				class.Properties[0] = ctorProp
				break
			}
		}
	}

	// Pack the class back into an expression. We don't need to handle TypeScript
	// decorators for class expressions because TypeScript doesn't support them.
	if kind == classKindExpr {
		// Initialize any remaining computed properties immediately after the end
		// of the class body
		if computedPropertyCache.Data != nil {
			expr = ast.JoinWithComma(expr, computedPropertyCache)
		}

		// Join the static field initializers if this is a class expression
		if len(staticFields) > 0 {
			for _, initializer := range staticFields {
				expr = ast.JoinWithComma(expr, initializer)
			}
			expr = ast.JoinWithComma(expr, nameFunc())
			if wrapFunc != nil {
				expr = wrapFunc(expr)
			}
		}
		return nil, expr
	}

	// Pack the class back into a statement, with potentially some extra
	// statements afterwards
	var stmts []ast.Stmt
	if len(class.TSDecorators) > 0 {
		name := nameFunc()
		id, _ := name.Data.(*ast.EIdentifier)
		classExpr := ast.EClass{Class: *class}
		class = &classExpr.Class
		stmts = append(stmts, ast.Stmt{classLoc, &ast.SLocal{
			Kind:     ast.LocalLet,
			IsExport: kind == classKindExportStmt,
			Decls: []ast.Decl{ast.Decl{
				Binding: ast.Binding{name.Loc, &ast.BIdentifier{id.Ref}},
				Value:   &ast.Expr{classLoc, &classExpr},
			}},
		}})
	} else {
		switch kind {
		case classKindStmt:
			stmts = append(stmts, ast.Stmt{classLoc, &ast.SClass{Class: *class}})
		case classKindExportStmt:
			stmts = append(stmts, ast.Stmt{classLoc, &ast.SClass{Class: *class, IsExport: true}})
		case classKindExportDefaultStmt:
			stmts = append(stmts, ast.Stmt{classLoc, &ast.SExportDefault{
				DefaultName: defaultName,
				Value:       ast.ExprOrStmt{Stmt: &ast.Stmt{classLoc, &ast.SClass{Class: *class}}},
			}})
		}
	}

	// The official TypeScript compiler adds generated code after the class body
	// in this exact order. Matching this order is important for correctness.
	if computedPropertyCache.Data != nil {
		stmts = append(stmts, ast.Stmt{expr.Loc, &ast.SExpr{computedPropertyCache}})
	}
	for _, expr := range staticFields {
		stmts = append(stmts, ast.Stmt{expr.Loc, &ast.SExpr{expr}})
	}
	for _, expr := range instanceDecorators {
		stmts = append(stmts, ast.Stmt{expr.Loc, &ast.SExpr{expr}})
	}
	for _, expr := range staticDecorators {
		stmts = append(stmts, ast.Stmt{expr.Loc, &ast.SExpr{expr}})
	}
	if len(class.TSDecorators) > 0 {
		stmts = append(stmts, ast.Stmt{expr.Loc, &ast.SExpr{ast.Expr{classLoc, &ast.EBinary{
			Op:   ast.BinOpAssign,
			Left: nameFunc(),
			Right: p.callRuntime(classLoc, "__decorate", []ast.Expr{
				ast.Expr{classLoc, &ast.EArray{Items: class.TSDecorators}},
				nameFunc(),
			}),
		}}}})
		if kind == classKindExportDefaultStmt {
			// Generate a new default name symbol since the current one is being used
			// by the class. If this SExportDefault turns into a variable declaration,
			// we don't want it to accidentally use the same variable as the class and
			// cause a name collision.
			nameFromPath := ast.GenerateNonUniqueNameFromPath(p.source.AbsolutePath) + "_default"
			defaultRef := p.generateTempRef(tempRefNoDeclare)
			p.symbols[defaultRef.InnerIndex].Name = nameFromPath
			p.namedExports["default"] = defaultRef
			p.recordDeclaredSymbol(defaultRef)

			name := nameFunc()
			stmts = append(stmts, ast.Stmt{classLoc, &ast.SExportDefault{
				DefaultName: ast.LocRef{defaultName.Loc, defaultRef},
				Value:       ast.ExprOrStmt{Expr: &name},
			}})
		}
		class.Name = nil
	}
	return stmts, ast.Expr{}
}