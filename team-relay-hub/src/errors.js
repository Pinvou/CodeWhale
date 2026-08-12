export class AppError extends Error {
  constructor(status, code, message, details = undefined) {
    super(message);
    this.name = "AppError";
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

export function invalidRequest(message, details) {
  return new AppError(400, "invalid_request", message, details);
}

export function asAppError(error) {
  if (error instanceof AppError) return error;
  return new AppError(503, "service_unavailable", "A required service is unavailable.");
}
