import type { Records } from "./port";

export function runRecordsContract(create: () => Records): void {
  void create;
}
