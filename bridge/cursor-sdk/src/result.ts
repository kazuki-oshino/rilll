export function resolveFinalResult(streamedResult: string, waitResult: string | undefined): string {
  return waitResult ?? streamedResult;
}
