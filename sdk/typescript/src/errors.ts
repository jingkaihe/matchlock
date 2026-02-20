export class MatchlockError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "MatchlockError";
  }
}

export class RPCError extends MatchlockError {
  static readonly PARSE_ERROR = -32700;
  static readonly INVALID_REQUEST = -32600;
  static readonly METHOD_NOT_FOUND = -32601;
  static readonly INVALID_PARAMS = -32602;
  static readonly INTERNAL_ERROR = -32603;
  static readonly VM_FAILED = -32000;
  static readonly EXEC_FAILED = -32001;
  static readonly FILE_FAILED = -32002;
  static readonly CANCELLED = -32003;

  readonly code: number;
  readonly rpcMessage: string;

  constructor(code: number, message: string) {
    super(`[${code}] ${message}`);
    this.name = "RPCError";
    this.code = code;
    this.rpcMessage = message;
  }

  isVMError(): boolean {
    return this.code === RPCError.VM_FAILED;
  }

  isExecError(): boolean {
    return this.code === RPCError.EXEC_FAILED;
  }

  isFileError(): boolean {
    return this.code === RPCError.FILE_FAILED;
  }
}
