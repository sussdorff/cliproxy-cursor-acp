import { runRecordsContract } from "../src/ports/records/contract";
import { MemoryRecords } from "../src/records/memory";

runRecordsContract(() => new MemoryRecords());
