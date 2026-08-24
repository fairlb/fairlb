import { useState } from "react";

/**
 * useCursorStack 是「上一页 / 下一页」这一种分页的唯一实现。
 *
 * # 它和 useCursorList 不是同一件事
 *
 * `useCursorList` 累积：读者要的是「一直往下看」，行只增不减。这个钩子**换页**：
 * 读者要的是「翻到我要找的那一条」，一次只在一页上。两者的失败方式也不同——前者
 * 怕重复行，后者怕丢了回上一页的出口。它们各自命名、各自住在这里，feature 文件
 * 不再手搓（ADR-0196）。
 *
 * # 为什么是一摞游标而不是一个页码
 *
 * 游标**就是位置**。记页码要求「第 N 页」可由 N 算出，而游标分页没有这个映射；
 * 压栈与弹栈则天然对应前进与后退。
 *
 * # 换筛选条件必须回第一页
 *
 * 第五页的游标属于上一个结果集，带着它翻会翻到一批不相干的行里去——而且看不出来，
 * 因为那些行本身是合法的。`reset()` 是筛选变更时的那一步。
 */
export function useCursorStack(): {
  /** 当前页的游标；首页是 undefined。 */
  cursor: string | undefined;
  /** 从 1 起的页码，只用于显示。 */
  page: number;
  /** 是否在首页——「上一页」按钮的禁用条件。 */
  atFirst: boolean;
  next: (cursor: string) => void;
  prev: () => void;
  reset: () => void;
} {
  const [stack, setStack] = useState<string[]>([]);
  return {
    cursor: stack.length > 0 ? stack[stack.length - 1] : undefined,
    page: stack.length + 1,
    atFirst: stack.length === 0,
    next: (cursor: string) => setStack((s) => [...s, cursor]),
    prev: () => setStack((s) => s.slice(0, -1)),
    reset: () => setStack([]),
  };
}
