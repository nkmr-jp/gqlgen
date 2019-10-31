package handler

import (
	"context"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/ast"
	"github.com/vektah/gqlparser/gqlerror"
	"github.com/vektah/gqlparser/parser"
	"github.com/vektah/gqlparser/validator"
)

type executor struct {
	operationMiddleware    graphql.OperationHandler
	resultHandler          graphql.ResponseMiddleware
	responseMiddleware     graphql.FieldMiddleware
	requestParamMutators   []graphql.RequestParameterMutator
	requestContextMutators []graphql.RequestContextMutator
	server                 *Server
}

var _ graphql.GraphExecutor = executor{}

func newExecutor(s *Server) executor {
	e := executor{
		server: s,
	}
	e.operationMiddleware = e.executableSchemaHandler
	e.resultHandler = func(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
		return next(ctx)
	}
	e.responseMiddleware = func(ctx context.Context, next graphql.Resolver) (res interface{}, err error) {
		return next(ctx)
	}

	// this loop goes backwards so the first plugin is the outer most middleware and runs first.
	for i := len(s.plugins) - 1; i >= 0; i-- {
		p := s.plugins[i]
		if p, ok := p.(graphql.OperationInterceptor); ok {
			previous := e.operationMiddleware
			e.operationMiddleware = func(ctx context.Context, writer graphql.Writer) {
				p.InterceptOperation(ctx, previous, writer)
			}
		}

		if p, ok := p.(graphql.ResponseInterceptor); ok {
			previous := e.resultHandler
			e.resultHandler = func(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
				return p.InterceptResponse(ctx, func(ctx context.Context) *graphql.Response {
					return previous(ctx, next)
				})
			}
		}

		if p, ok := p.(graphql.FieldInterceptor); ok {
			previous := e.responseMiddleware
			e.responseMiddleware = func(ctx context.Context, next graphql.Resolver) (res interface{}, err error) {
				return p.InterceptField(ctx, func(ctx context.Context) (res interface{}, err error) {
					return previous(ctx, next)
				})
			}
		}
	}

	for _, p := range s.plugins {
		if p, ok := p.(graphql.RequestParameterMutator); ok {
			e.requestParamMutators = append(e.requestParamMutators, p)
		}

		if p, ok := p.(graphql.RequestContextMutator); ok {
			e.requestContextMutators = append(e.requestContextMutators, p)
		}

	}

	return e
}

func (e executor) DispatchRequest(ctx context.Context, writer graphql.Writer) {
	e.operationMiddleware(ctx, writer)
}

func (e executor) CreateRequestContext(ctx context.Context, params *graphql.RawParams) (*graphql.RequestContext, gqlerror.List) {
	for _, p := range e.requestParamMutators {
		if err := p.MutateRequestParameters(ctx, params); err != nil {
			return nil, gqlerror.List{err}
		}
	}

	var gerr *gqlerror.Error

	rc := &graphql.RequestContext{
		DisableIntrospection: true,
		Recover:              graphql.DefaultRecover,
		ResolverMiddleware:   e.responseMiddleware,
		ComplexityLimit:      0,
		RawQuery:             params.Query,
		OperationName:        params.OperationName,
		Variables:            params.Variables,
	}
	rc.Stats.OperationStart = graphql.GetStartTime(ctx)

	rc.Doc, gerr = e.parseOperation(ctx, rc)
	if gerr != nil {
		return nil, []*gqlerror.Error{gerr}
	}

	ctx, op, listErr := e.validateOperation(ctx, rc)
	if len(listErr) != 0 {
		return nil, listErr
	}

	vars, err := validator.VariableValues(e.server.es.Schema(), op, rc.Variables)
	if err != nil {
		return nil, gqlerror.List{err}
	}

	rc.Variables = vars

	for _, p := range e.requestContextMutators {
		if err := p.MutateRequestContext(ctx, rc); err != nil {
			return nil, gqlerror.List{err}
		}
	}

	return rc, nil
}

func (e *executor) write(ctx context.Context, resp *graphql.Response, writer graphql.Writer) {
	resp.Extensions = graphql.GetExtensions(ctx)

	for _, err := range graphql.GetErrors(ctx) {
		resp.Errors = append(resp.Errors, err)
	}
	writer(getStatus(resp), resp)
}

// executableSchemaHandler is the inner most operation handler, it invokes the graph directly after all middleware
// and sends responses to the transport so it can be returned to the client
func (e *executor) executableSchemaHandler(ctx context.Context, write graphql.Writer) {
	rc := graphql.GetRequestContext(ctx)

	op := rc.Doc.Operations.ForName(rc.OperationName)

	switch op.Operation {
	case ast.Query:
		resCtx := graphql.WithResponseContext(ctx, e.server.errorPresenter, e.server.recoverFunc)
		resp := e.resultHandler(resCtx, func(ctx context.Context) *graphql.Response {
			return e.server.es.Query(ctx, op)
		})
		e.write(resCtx, resp, write)

	case ast.Mutation:
		resCtx := graphql.WithResponseContext(ctx, e.server.errorPresenter, e.server.recoverFunc)
		resp := e.resultHandler(resCtx, func(ctx context.Context) *graphql.Response {
			return e.server.es.Mutation(ctx, op)
		})
		e.write(resCtx, resp, write)

	case ast.Subscription:
		responses := e.server.es.Subscription(ctx, op)
		for {
			resCtx := graphql.WithResponseContext(ctx, e.server.errorPresenter, e.server.recoverFunc)
			resp := e.resultHandler(resCtx, func(ctx context.Context) *graphql.Response {
				resp := responses()
				if resp == nil {
					return nil
				}
				resp.Extensions = graphql.GetExtensions(ctx)
				return resp
			})
			if resp == nil {
				break
			}
			e.write(resCtx, resp, write)
		}

	default:
		write(graphql.StatusValidationError, graphql.ErrorResponse(ctx, "unsupported GraphQL operation"))
	}
}

func (e executor) parseOperation(ctx context.Context, rc *graphql.RequestContext) (*ast.QueryDocument, *gqlerror.Error) {
	rc.Stats.Parsing.Start = graphql.Now()
	defer func() {
		rc.Stats.Parsing.End = graphql.Now()
	}()
	return parser.ParseQuery(&ast.Source{Input: rc.RawQuery})
}

func (e executor) validateOperation(ctx context.Context, rc *graphql.RequestContext) (context.Context, *ast.OperationDefinition, gqlerror.List) {
	rc.Stats.Validation.Start = graphql.Now()
	defer func() {
		rc.Stats.Validation.End = graphql.Now()
	}()

	listErr := validator.Validate(e.server.es.Schema(), rc.Doc)
	if len(listErr) != 0 {
		return ctx, nil, listErr
	}

	op := rc.Doc.Operations.ForName(rc.OperationName)
	if op == nil {
		return ctx, nil, gqlerror.List{gqlerror.Errorf("operation %s not found", rc.OperationName)}
	}

	return ctx, op, nil
}