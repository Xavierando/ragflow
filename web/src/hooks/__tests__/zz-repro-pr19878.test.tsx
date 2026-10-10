// Reproductions for the review of PR #19878. Not part of the PR.
import { documentFilter } from '@/services/knowledge-service';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { DocumentKeys } from '../document-query-keys';
import { useHandleSearchChange } from '../logic-hooks';
import { useGetDocumentFilter } from '../use-document-request';

jest.mock('@/services/knowledge-service', () => ({
  documentFilter: jest.fn(),
}));
jest.mock('@/routes', () => ({
  Routes: { DataflowResult: '/dataflow-result' },
}));

const mockDocumentFilter = jest.mocked(documentFilter);
const createClient = () =>
  new QueryClient({ defaultOptions: { queries: { retry: false } } });
let queryClient = createClient();

const wrapper = ({ children }: { children: React.ReactNode }) => (
  <MemoryRouter initialEntries={['/dataset/files/dataset-id']}>
    <Routes>
      <Route
        path="/dataset/files/:id"
        element={
          <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
        }
      />
    </Routes>
  </MemoryRouter>
);

const respond = (suffix: Record<string, number>) =>
  mockDocumentFilter.mockResolvedValue({
    data: { code: 0, data: { filter: { suffix } } },
  } as never);

beforeEach(() => {
  queryClient = createClient();
  mockDocumentFilter.mockReset();
  respond({ pdf: 1 });
});

// Finding 3: after a document mutation, reopening the popover shows fresh counts.
it('repro 3: counts refresh after a document mutation', async () => {
  const { result } = renderHook(() => useGetDocumentFilter(), { wrapper });
  act(() => result.current.onOpenChange(true));
  await waitFor(() =>
    expect(result.current.filter.suffix).toEqual({ pdf: 1 }),
  );

  // A .docx is uploaded. Every document mutation in use-document-request.ts
  // invalidates DocumentKeys.all(); this is that call.
  respond({ pdf: 1, docx: 1 });
  await act(async () => {
    await queryClient.invalidateQueries({ queryKey: DocumentKeys.all() });
  });

  // Reopen within the 30 s window.
  act(() => {
    result.current.onOpenChange(false);
    result.current.onOpenChange(true);
  });
  await new Promise((r) => setTimeout(r, 200));

  console.log(
    `repro 3: documentFilter calls=${mockDocumentFilter.mock.calls.length}, suffix shown=${JSON.stringify(result.current.filter.suffix)}`,
  );
  expect(result.current.filter.suffix).toEqual({ pdf: 1, docx: 1 });
});

// Finding 4: typing in the file list's search box refetches the counts.
it('repro 4: search keystrokes refetch the counts', async () => {
  const { result } = renderHook(
    () => ({ search: useHandleSearchChange(), filter: useGetDocumentFilter() }),
    { wrapper },
  );
  act(() => result.current.filter.onOpenChange(true));
  await waitFor(() => expect(mockDocumentFilter).toHaveBeenCalledTimes(1));

  for (const value of ['r', 'rep', 'report']) {
    act(() =>
      result.current.search.handleInputChange({
        target: { value },
      } as React.ChangeEvent<HTMLInputElement>),
    );
    await new Promise((r) => setTimeout(r, 600)); // past the 500 ms debounce
  }

  const keys = queryClient
    .getQueryCache()
    .findAll({ queryKey: DocumentKeys.allFilters() })
    .map((q) => JSON.stringify(q.queryKey));
  console.log(
    `repro 4: documentFilter calls=${mockDocumentFilter.mock.calls.length}, filter cache keys=${keys.join(' ')}`,
  );
  // The finding claims >1 call; this asserts that claim.
  expect(mockDocumentFilter.mock.calls.length).toBeGreaterThan(1);
}, 10000);
