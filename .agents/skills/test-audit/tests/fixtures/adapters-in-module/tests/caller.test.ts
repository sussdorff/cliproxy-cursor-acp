import type { Records } from "../src/ports/records/port";
import { MemoryRecords } from "../src/records/memory";

const records: Records = new MemoryRecords();
void records;
