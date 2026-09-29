import React from 'react';
export const ContainerPage: React.FC<{ org: string; endpoint: string; container: string }> = ({ container }) => <div className="ky-page"><h1>{container.slice(0, 12)}</h1></div>;
