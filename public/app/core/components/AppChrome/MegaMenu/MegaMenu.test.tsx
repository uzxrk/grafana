import { render, screen } from '@testing-library/react';
import React from 'react';
import { Router } from 'react-router-dom';
import { getGrafanaContextMock } from 'test/mocks/getGrafanaContextMock';

import { NavModelItem } from '@grafana/data';
import { selectors } from '@grafana/e2e-selectors';
import { locationService } from '@grafana/runtime';

import { TestProvider } from '../../../../../test/helpers/TestProvider';

import { MegaMenu } from './MegaMenu';

const setup = () => {
  const navBarTree: NavModelItem[] = [
    {
      text: 'Section name',
      id: 'section',
      url: 'section',
      children: [
        { text: 'Child1', id: 'child1', url: 'section/child1' },
        { text: 'Child2', id: 'child2', url: 'section/child2' },
      ],
    },
    {
      text: 'Profile',
      id: 'profile',
      url: 'profile',
    },
  ];

  const grafanaContext = getGrafanaContextMock();
  grafanaContext.chrome.setMegaMenuOpen(true);

  return render(
    <TestProvider storeState={{ navBarTree }} grafanaContext={grafanaContext}>
      <Router history={locationService.getHistory()}>
        <MegaMenu onClose={() => {}} />
      </Router>
    </TestProvider>
  );
};

describe('MegaMenu', () => {
  it('should render component', async () => {
    setup();

    expect(await screen.findByTestId(selectors.components.NavMenu.Menu)).toBeInTheDocument();
    expect(await screen.findByRole('link', { name: 'Section name' })).toBeInTheDocument();
  });

  it('should filter out profile', async () => {
    setup();

    expect(screen.queryByLabelText('Profile')).not.toBeInTheDocument();
  });
});
