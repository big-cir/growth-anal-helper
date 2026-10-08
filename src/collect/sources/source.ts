// 소스 어댑터: 결과를 행 단위로 흘려준다.
export type RawValue = string | null;

export type SelectResult = { columns: string[]; rows: number; ms: number };

export interface SourceAdapter {
  /** text 칸에 's' 접두사를 붙여야 하는지 */
  readonly wrapText: boolean;
  now(): Promise<string>;
  /** 첫 행 전에 onColumns, 행마다 onRow. 콜백이 던지면 멈춘다 */
  selectStream(sql: string, onRow: (values: RawValue[]) => void, onColumns?: (columns: string[]) => void): Promise<SelectResult>;
  abort(): void;
}
