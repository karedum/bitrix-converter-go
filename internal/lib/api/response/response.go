package response

type Response struct {
	Success bool    `json:"success"`
	Result  *Result `json:"result,omitempty"`
}

type Result struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func Error(msg string, code int) Response {
	return Response{
		Success: false,
		Result:  &Result{Code: code, Msg: msg},
	}
}

func Success() Response {
	return Response{
		Success: true,
	}
}
